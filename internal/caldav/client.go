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

// Client fetches and renders the upstream calendar.
type Client struct {
	cfg        *config.Config
	dav        *caldav.Client
	httpClient webdav.HTTPClient
	calURL     string // resolved calendar collection path
}

// New constructs a Client with a basic-auth HTTP client targeting the upstream
// server. The calendar path is resolved lazily on the first Fetch.
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
		calURL:     cfg.CalendarPath,
	}, nil
}

// resolveCalendar determines which calendar collection to query, using the
// configured path when present and falling back to CalDAV discovery otherwise.
func (c *Client) resolveCalendar(ctx context.Context) (string, error) {
	if c.calURL != "" {
		slog.Debug("using configured calendar path", "path", c.calURL)
		return c.calURL, nil
	}

	slog.Debug("discovering calendar via CalDAV")

	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return "", fmt.Errorf("discover principal: %w", err)
	}
	slog.Debug("discovered current-user-principal", "principal", principal)

	homeSet, err := c.dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return "", fmt.Errorf("discover calendar home set: %w", err)
	}
	slog.Debug("discovered calendar home set", "homeSet", homeSet)

	cals, err := c.dav.FindCalendars(ctx, homeSet)
	if err != nil {
		return "", fmt.Errorf("list calendars: %w", err)
	}
	if len(cals) == 0 {
		return "", fmt.Errorf("no calendars found at %q", homeSet)
	}

	for _, cal := range cals {
		slog.Debug("found calendar", "name", cal.Name, "path", cal.Path)
	}

	if len(cals) > 1 {
		slog.Warn("multiple calendars discovered, using the first",
			"count", len(cals),
			"chosen", cals[0].Path,
			"hint", "set CALDAV_CALENDAR_PATH to pick another")
	}

	c.calURL = cals[0].Path
	slog.Info("resolved calendar", "path", c.calURL)
	return c.calURL, nil
}

// Fetch reads all events within the configured time window and returns the
// merged calendar encoded as an .ics document.
func (c *Client) Fetch(ctx context.Context) ([]byte, error) {
	calPath, err := c.resolveCalendar(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	start := now.Add(-c.cfg.QueryWindowPast)
	end := now.Add(c.cfg.QueryWindowFuture)

	slog.Debug("querying calendar", "path", calPath, "rangeStart", start, "rangeEnd", end)

	// We intentionally do not use c.dav.QueryCalendar here.
	//
	// Server returns DAV:getetag values as bare numeric strings like:
	//   <D:getetag>1782304664085</D:getetag>
	//
	// The upstream go-webdav CalDAV query path expects a quoted HTTP ETag and
	// fails while unquoting it. Discovery works fine, so we keep discovery from
	// the library and perform the REPORT request ourselves.
	cals, err := c.queryCalendarRaw(ctx, calPath, start, end)
	if err != nil {
		return nil, fmt.Errorf("query calendar %q: %w", calPath, err)
	}

	return Merge(cals)
}

// queryCalendarRaw performs a CalDAV calendar-query REPORT manually via
// net/http and decodes the returned calendar-data payloads.
//
// This avoids the strict ETag parsing in the high-level library while keeping
// the rest of the client logic unchanged.
func (c *Client) queryCalendarRaw(ctx context.Context, calPath string, start, end time.Time) ([]*ical.Calendar, error) {
	baseURL, err := url.Parse(c.cfg.RemoteURL)
	if err != nil {
		return nil, fmt.Errorf("parse remote URL %q: %w", c.cfg.RemoteURL, err)
	}

	rel, err := url.Parse(calPath)
	if err != nil {
		return nil, fmt.Errorf("parse calendar path %q: %w", calPath, err)
	}
	reqURL := baseURL.ResolveReference(rel)

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

	slog.Debug("query returned objects", "count", len(cals))
	return cals, nil
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

// Merge combines the components of several calendars into one .ics document.
// VEVENT and VTODO components are copied verbatim; VTIMEZONE components are
// deduplicated by their TZID so shared timezones appear only once. The result
// is a valid standalone iCalendar with our own PRODID/VERSION.
func Merge(cals []*ical.Calendar) ([]byte, error) {
	out := ical.NewCalendar()
	out.Props.SetText(ical.PropVersion, "2.0")
	out.Props.SetText(ical.PropProductID, prodID)

	seenTZ := make(map[string]bool)
	var components, timezones, dupTZ int

	for _, cal := range cals {
		if cal == nil {
			continue
		}
		for _, child := range cal.Children {
			switch child.Name {
			case ical.CompTimezone:
				tzid := propValue(child.Props.Get(ical.PropTimezoneID))
				if tzid != "" && seenTZ[tzid] {
					dupTZ++
					slog.Debug("skipping duplicate timezone", "tzid", tzid)
					continue
				}
				if tzid != "" {
					seenTZ[tzid] = true
				}
				out.Children = append(out.Children, child)
				timezones++
			case ical.CompEvent, ical.CompToDo:
				out.Children = append(out.Children, child)
				components++
			}
		}
	}

	slog.Debug("merged calendar",
		"sourceCalendars", len(cals),
		"components", components,
		"timezones", timezones,
		"duplicateTimezonesDropped", dupTZ)

	if components == 0 {
		slog.Debug("no events after merge, returning empty calendar")
		return emptyCalendar, nil
	}

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(out); err != nil {
		return nil, fmt.Errorf("encode merged calendar: %w", err)
	}
	return buf.Bytes(), nil
}

// propValue safely returns a property's value, or "" if the property is absent.
func propValue(p *ical.Prop) string {
	if p == nil {
		return ""
	}
	return p.Value
}
