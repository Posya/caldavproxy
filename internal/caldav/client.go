// Package caldav reads an authenticated upstream CalDAV calendar and renders it
// into a single iCalendar (.ics) document.
package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"caldavproxy/internal/config"
)

// prodID identifies this application as the producer of the merged calendar.
const prodID = "-//caldavproxy//CalDav feed//EN"

// emptyCalendar is served when the upstream calendar has no events. The go-ical
// encoder rejects calendars with zero components, so we emit a minimal but
// valid VCALENDAR by hand.
var emptyCalendar = []byte("BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:" + prodID + "\r\n" +
	"END:VCALENDAR\r\n")

// Client fetches and renders upstream calendar(s).
type Client struct {
	cfg        *config.Config
	dav        *caldav.Client
	httpClient webdav.HTTPClient
	// discovered caches auto-discovered calendar paths when no sources are configured.
	discovered []string
}

// New constructs a Client with a basic-auth HTTP client targeting the upstream
// server. Calendar sources are resolved on Fetch (configured list or discovery).
func New(cfg *config.Config) (*Client, error) {
	httpClient := webdav.HTTPClientWithBasicAuth(http.DefaultClient, cfg.Username, cfg.Password)
	dav, err := caldav.NewClient(httpClient, cfg.RemoteURL)
	if err != nil {
		return nil, fmt.Errorf("create caldav client: %w", err)
	}
	return &Client{
		cfg:        cfg,
		dav:        dav,
		httpClient: httpClient,
	}, nil
}

// resolveSources returns the calendar collection paths/URLs to query.
func (c *Client) resolveSources(ctx context.Context) ([]string, error) {
	if len(c.cfg.CalendarSources) > 0 {
		slog.Debug("using configured calendar sources",
			"count", len(c.cfg.CalendarSources),
			"sources", c.cfg.CalendarSources)
		return c.cfg.CalendarSources, nil
	}
	if len(c.discovered) > 0 {
		return c.discovered, nil
	}

	slog.Debug("discovering calendar via CalDAV")

	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover principal: %w", err)
	}
	slog.Debug("discovered current-user-principal", "principal", principal)

	homeSet, err := c.dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("discover calendar home set: %w", err)
	}
	slog.Debug("discovered calendar home set", "homeSet", homeSet)

	cals, err := c.dav.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	if len(cals) == 0 {
		return nil, fmt.Errorf("no calendars found at %q", homeSet)
	}

	paths := make([]string, 0, len(cals))
	for _, cal := range cals {
		slog.Debug("found calendar", "name", cal.Name, "path", cal.Path)
		if cal.Path == "" {
			continue
		}
		paths = append(paths, cal.Path)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no calendar paths found at %q", homeSet)
	}

	c.discovered = paths
	slog.Info("resolved calendars", "count", len(paths), "paths", paths)
	return c.discovered, nil
}

// Fetch reads events from every configured (or discovered) calendar within the
// time window and returns one merged agenda .ics document.
func (c *Client) Fetch(ctx context.Context) ([]byte, error) {
	sources, err := c.resolveSources(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	start := now.Add(-c.cfg.QueryWindowPast)
	end := now.Add(c.cfg.QueryWindowFuture)

	var (
		all      []*ical.Calendar
		failures []string
		okCount  int
	)

	for i, src := range sources {
		slog.Debug("querying calendar", "source", src, "index", i, "rangeStart", start, "rangeEnd", end)

		cals, err := c.queryCalendarRaw(ctx, src, start, end)
		if err != nil {
			slog.Error("query calendar failed", "source", src, "error", err)
			failures = append(failures, fmt.Sprintf("%s: %v", src, err))
			continue
		}

		prefixUIDs(cals, fmt.Sprintf("s%d", i))
		all = append(all, cals...)
		okCount++
		slog.Debug("calendar source ok", "source", src, "objects", len(cals))
	}

	if okCount == 0 {
		if len(failures) == 0 {
			return nil, fmt.Errorf("no calendar sources configured")
		}
		return nil, fmt.Errorf("all calendar sources failed (%d): %s", len(failures), strings.Join(failures, "; "))
	}
	if len(failures) > 0 {
		slog.Warn("some calendar sources failed; continuing with partial feed",
			"ok", okCount, "failed", len(failures), "errors", failures)
	}

	slog.Debug("fetch complete", "sourcesOK", okCount, "sourcesFailed", len(failures), "objects", len(all))
	return Merge(all, start, end)
}

// prefixUIDs namespaces event UIDs with a per-source prefix so identical UIDs
// from different calendars do not collide in the flattened agenda.
func prefixUIDs(cals []*ical.Calendar, prefix string) {
	for _, cal := range cals {
		if cal == nil {
			continue
		}
		for _, child := range cal.Children {
			if child.Name != ical.CompEvent && child.Name != ical.CompToDo {
				continue
			}
			uid := propValue(child.Props.Get(ical.PropUID))
			if uid == "" || strings.HasPrefix(uid, prefix+"|") {
				continue
			}
			child.Props.SetText(ical.PropUID, prefix+"|"+uid)
		}
	}
}

// queryCalendarRaw performs a CalDAV calendar-query REPORT manually via
// net/http and decodes the returned calendar-data payloads.
//
// This avoids the strict ETag parsing in the high-level library while keeping
// the rest of the client logic unchanged.
//
// pathOrURL may be an absolute http(s) URL or a path relative to RemoteURL.
func (c *Client) queryCalendarRaw(ctx context.Context, pathOrURL string, start, end time.Time) ([]*ical.Calendar, error) {
	reqURL, err := resolveCalendarURL(c.cfg.RemoteURL, pathOrURL)
	if err != nil {
		return nil, err
	}

	reportBody := buildCalendarQueryBody(start, end)

	req, err := http.NewRequestWithContext(ctx, "REPORT", reqURL.String(), strings.NewReader(reportBody))
	if err != nil {
		return nil, fmt.Errorf("build REPORT request: %w", err)
	}

	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	req.Header.Set("Depth", "1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform REPORT request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read REPORT response: %w", err)
	}

	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("unexpected REPORT status: %s: %s", resp.Status, string(respBody))
	}

	var ms multistatus
	if err := xml.Unmarshal(respBody, &ms); err != nil {
		return nil, fmt.Errorf("decode multistatus XML: %w", err)
	}

	cals := make([]*ical.Calendar, 0, len(ms.Responses))

	for _, r := range ms.Responses {
		var etag string
		var calData string

		for _, ps := range r.PropStats {
			if !propstatOK(ps.Status) {
				continue
			}

			// Server may return a bare numeric ETag without HTTP quotes.
			// Treat it as an opaque string and do not try to unquote or validate
			// it as a strict HTTP header value.
			if ps.Prop.GetETag != "" {
				etag = strings.TrimSpace(ps.Prop.GetETag)
			}
			if ps.Prop.CalendarData != "" {
				calData = ps.Prop.CalendarData
			}
		}

		if calData == "" {
			slog.Debug("skipping object with no calendar-data", "path", r.Href, "etag", etag)
			continue
		}

		cal, err := ical.NewDecoder(strings.NewReader(calData)).Decode()
		if err != nil {
			return nil, fmt.Errorf("decode calendar object %q (etag=%q): %w", r.Href, etag, err)
		}

		slog.Debug("calendar object", "path", r.Href, "etag", etag, "events", len(cal.Events()))
		cals = append(cals, cal)
	}

	slog.Debug("query returned objects", "count", len(cals), "url", reqURL.String())
	return cals, nil
}

// resolveCalendarURL joins RemoteURL with a relative path, or returns an
// absolute http(s) calendar URL as-is.
func resolveCalendarURL(remoteURL, pathOrURL string) (*url.URL, error) {
	pathOrURL = strings.TrimSpace(pathOrURL)
	if pathOrURL == "" {
		return nil, fmt.Errorf("empty calendar path/URL")
	}

	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		u, err := url.Parse(pathOrURL)
		if err != nil {
			return nil, fmt.Errorf("parse calendar URL %q: %w", pathOrURL, err)
		}
		return u, nil
	}

	baseURL, err := url.Parse(remoteURL)
	if err != nil {
		return nil, fmt.Errorf("parse remote URL %q: %w", remoteURL, err)
	}
	rel, err := url.Parse(pathOrURL)
	if err != nil {
		return nil, fmt.Errorf("parse calendar path %q: %w", pathOrURL, err)
	}
	return baseURL.ResolveReference(rel), nil
}

// buildCalendarQueryBody returns a REPORT body requesting ETag and iCalendar
// payloads for VEVENT components that intersect the provided time range.
func buildCalendarQueryBody(start, end time.Time) string {
	startStr := start.UTC().Format("20060102T150405Z")
	endStr := end.UTC().Format("20060102T150405Z")

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <D:getetag/>
    <C:calendar-data/>
  </D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="%s" end="%s"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`, startStr, endStr)
}

// propstatOK reports whether a DAV propstat status is successful enough for us
// to consume its properties. In practice we only care about 200 responses.
func propstatOK(status string) bool {
	return strings.Contains(status, " 200 ")
}

// multistatus is the top-level XML container for a WebDAV/CALDAV 207 response.
type multistatus struct {
	XMLName   xml.Name   `xml:"DAV: multistatus"`
	Responses []response `xml:"response"`
}

// response represents one DAV response item, typically one .ics object.
type response struct {
	Href      string     `xml:"href"`
	PropStats []propstat `xml:"propstat"`
}

// propstat contains a set of properties and the HTTP status for that property
// group.
type propstat struct {
	Prop   prop   `xml:"prop"`
	Status string `xml:"status"`
}

// prop contains just the DAV/CALDAV properties we need from the REPORT
// response. Unknown properties are ignored by encoding/xml automatically.
type prop struct {
	GetETag      string `xml:"getetag"`
	CalendarData string `xml:"calendar-data"`
}

// Merge combines the components of several calendars into one .ics document,
// then flattens them into an agenda feed: one VEVENT per occurrence in
// [start, end), without RRULE/RECURRENCE-ID, with UTC timestamps. The result
// is a standalone iCalendar with our own PRODID/VERSION and RFC 5545 folding.
func Merge(cals []*ical.Calendar, start, end time.Time) ([]byte, error) {
	out := ical.NewCalendar()
	out.Props.SetText(ical.PropVersion, "2.0")
	out.Props.SetText(ical.PropProductID, prodID)

	var components int

	for _, cal := range cals {
		if cal == nil {
			continue
		}
		for _, child := range cal.Children {
			switch child.Name {
			case ical.CompEvent, ical.CompToDo:
				out.Children = append(out.Children, child)
				components++
			}
		}
	}

	slog.Debug("merged calendar before flatten",
		"sourceCalendars", len(cals),
		"components", components)

	if components == 0 {
		slog.Debug("no events after merge, returning empty calendar")
		return emptyCalendar, nil
	}

	kept, source := flattenCalendar(out, start, end)
	if kept == 0 {
		slog.Debug("no events in feed window after flatten, returning empty calendar",
			"sourceComponents", source)
		return emptyCalendar, nil
	}

	logMergedInventory(out)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(out); err != nil {
		return nil, fmt.Errorf("encode merged calendar: %w", err)
	}

	// go-ical does not fold long content lines; normalize to RFC 5545 limits.
	return foldICS(buf.Bytes()), nil
}

// logMergedInventory writes per-component details and aggregates that help
// investigate missing events (titles, dates, UID collisions, per-day counts).
// Individual components are logged at debug; colliding UIDs are warned about.
func logMergedInventory(cal *ical.Calendar) {
	type uidStat struct {
		total   int
		masters int // components without RECURRENCE-ID
	}

	byUID := make(map[string]*uidStat)
	byDay := make(map[string]int)

	for _, child := range cal.Children {
		if child.Name != ical.CompEvent && child.Name != ical.CompToDo {
			continue
		}

		uid := propValue(child.Props.Get(ical.PropUID))
		summary := propValue(child.Props.Get(ical.PropSummary))
		dtstart := propValue(child.Props.Get(ical.PropDateTimeStart))
		recID := propValue(child.Props.Get(ical.PropRecurrenceID))
		rrule := propValue(child.Props.Get(ical.PropRecurrenceRule))

		slog.Debug("merged component",
			"kind", child.Name,
			"uid", uid,
			"summary", summary,
			"dtstart", dtstart,
			"recurrenceId", recID,
			"rrule", rrule)

		if uid != "" {
			st := byUID[uid]
			if st == nil {
				st = &uidStat{}
				byUID[uid] = st
			}

			st.total++
			if recID == "" {
				st.masters++
			}
		}

		if day := eventDay(dtstart); day != "" {
			byDay[day]++
		}
	}

	if len(byDay) > 0 {
		days := make([]string, 0, len(byDay))
		for day := range byDay {
			days = append(days, day)
		}

		slices.Sort(days)

		parts := make([]string, 0, len(days))
		for _, day := range days {
			parts = append(parts, fmt.Sprintf("%s=%d", day, byDay[day]))
		}

		slog.Debug("events by day", "counts", strings.Join(parts, ","))
	}

	uids := make([]string, 0, len(byUID))
	for uid := range byUID {
		uids = append(uids, uid)
	}

	slices.Sort(uids)

	for _, uid := range uids {
		st := byUID[uid]
		if st.total <= 1 {
			continue
		}

		slog.Debug("UID used by multiple components",
			"uid", uid,
			"count", st.total,
			"masters", st.masters)

		// Several independent events sharing one UID (no RECURRENCE-ID) is
		// invalid iCalendar; many subscribers keep only one of them.
		if st.masters > 1 {
			slog.Warn("duplicate UID without RECURRENCE-ID",
				"uid", uid,
				"count", st.total,
				"masters", st.masters,
				"hint", "subscribers may keep only one event per UID")
		}
	}
}

// eventDay extracts YYYY-MM-DD from a DTSTART value (DATE or DATE-TIME).
func eventDay(dtstart string) string {
	if len(dtstart) < 8 {
		return ""
	}

	raw := dtstart[:8]
	for _, c := range raw {
		if c < '0' || c > '9' {
			return ""
		}
	}

	return raw[:4] + "-" + raw[4:6] + "-" + raw[6:8]
}

// propValue safely returns a property's value, or "" if the property is absent.
func propValue(p *ical.Prop) string {
	if p == nil {
		return ""
	}
	return p.Value
}
