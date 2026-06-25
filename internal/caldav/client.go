// Package caldav reads an authenticated upstream CalDAV calendar and renders it
// into a single iCalendar (.ics) document.
package caldav

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
	cfg    *config.Config
	dav    *caldav.Client
	calURL string // resolved calendar collection path
}

// New constructs a Client with a basic-auth HTTP client targeting the upstream
// server. The calendar path is resolved lazily on the first Fetch.
func New(cfg *config.Config) (*Client, error) {
	httpClient := webdav.HTTPClientWithBasicAuth(http.DefaultClient, cfg.Username, cfg.Password)
	dav, err := caldav.NewClient(httpClient, cfg.RemoteURL)
	if err != nil {
		return nil, fmt.Errorf("create caldav client: %w", err)
	}
	return &Client{cfg: cfg, dav: dav, calURL: cfg.CalendarPath}, nil
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
			"count", len(cals), "chosen", cals[0].Path,
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

	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			Comps: []caldav.CalendarCompRequest{
				{Name: ical.CompEvent, AllProps: true, AllComps: true},
			},
		},
		CompFilter: caldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []caldav.CompFilter{
				{Name: ical.CompEvent, Start: start, End: end},
			},
		},
	}

	objects, err := c.dav.QueryCalendar(ctx, calPath, query)
	if err != nil {
		return nil, fmt.Errorf("query calendar %q: %w", calPath, err)
	}
	slog.Debug("query returned objects", "count", len(objects))

	cals := make([]*ical.Calendar, 0, len(objects))
	for _, obj := range objects {
		if obj.Data == nil {
			slog.Debug("skipping object with no data", "path", obj.Path)
			continue
		}
		slog.Debug("calendar object", "path", obj.Path, "etag", obj.ETag, "events", len(obj.Data.Events()))
		cals = append(cals, obj.Data)
	}

	return Merge(cals)
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
