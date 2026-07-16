package caldav

import (
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// pruneCalendar trims the merged calendar for a "near-term" public feed:
//   - standalone VEVENT/VTODO outside [start, end) are dropped;
//   - recurring series keep the RRULE master (if still active) and only
//     RECURRENCE-ID overrides whose instance time falls in the window;
//   - ancient exception history is discarded so subscribers are not flooded
//     with dozens of VEVENTs sharing one UID.
func pruneCalendar(cal *ical.Calendar, start, end time.Time) (kept, dropped int) {
	if cal == nil {
		return 0, 0
	}

	var (
		timezones []*ical.Component
		others    []*ical.Component // VTODO and UID-less events after filtering
		byUID     = make(map[string][]*ical.Component)
		uidOrder  []string
	)

	for _, child := range cal.Children {
		switch child.Name {
		case ical.CompTimezone:
			timezones = append(timezones, child)
		case ical.CompToDo:
			if overlapsWindow(child, start, end) {
				others = append(others, child)
				kept++
			} else {
				dropped++
			}
		case ical.CompEvent:
			uid := propValue(child.Props.Get(ical.PropUID))
			if uid == "" {
				if overlapsWindow(child, start, end) {
					others = append(others, child)
					kept++
				} else {
					dropped++
				}
				continue
			}
			if _, ok := byUID[uid]; !ok {
				uidOrder = append(uidOrder, uid)
			}
			byUID[uid] = append(byUID[uid], child)
		default:
			others = append(others, child)
		}
	}

	var events []*ical.Component
	for _, uid := range uidOrder {
		selected := selectSeriesComponents(byUID[uid], start, end)
		kept += len(selected)
		dropped += len(byUID[uid]) - len(selected)
		events = append(events, selected...)
	}

	cal.Children = cal.Children[:0]
	cal.Children = append(cal.Children, timezones...)
	cal.Children = append(cal.Children, events...)
	cal.Children = append(cal.Children, others...)

	slog.Debug("pruned calendar for feed window",
		"windowStart", start,
		"windowEnd", end,
		"kept", kept,
		"dropped", dropped)

	return kept, dropped
}

// selectSeriesComponents chooses which components of one UID to publish.
func selectSeriesComponents(group []*ical.Component, start, end time.Time) []*ical.Component {
	var (
		master     *ical.Component
		exceptions []*ical.Component
		singles    []*ical.Component
	)

	for _, c := range group {
		hasRID := propValue(c.Props.Get(ical.PropRecurrenceID)) != ""
		hasRRULE := propValue(c.Props.Get(ical.PropRecurrenceRule)) != ""

		switch {
		case hasRID:
			exceptions = append(exceptions, c)
		case hasRRULE:
			// Prefer the first master if duplicates appear.
			if master == nil {
				master = c
			}
		default:
			singles = append(singles, c)
		}
	}

	// Pure non-recurring event(s) under this UID.
	if master == nil && len(exceptions) == 0 {
		out := make([]*ical.Component, 0, len(singles))
		for _, c := range singles {
			if overlapsWindow(c, start, end) {
				out = append(out, c)
			}
		}
		return out
	}

	keptExc := make([]*ical.Component, 0, len(exceptions))
	for _, c := range exceptions {
		if exceptionInWindow(c, start, end) {
			keptExc = append(keptExc, c)
		}
	}

	out := make([]*ical.Component, 0, 1+len(keptExc)+len(singles))
	if master != nil && seriesRelevant(master, start, len(keptExc) > 0) {
		out = append(out, master)
	}
	out = append(out, keptExc...)

	for _, c := range singles {
		if overlapsWindow(c, start, end) {
			out = append(out, c)
		}
	}

	return out
}

// seriesRelevant reports whether the RRULE master should be published.
func seriesRelevant(master *ical.Component, windowStart time.Time, hasExceptionInWindow bool) bool {
	if hasExceptionInWindow {
		return true
	}
	return seriesStillActive(master, windowStart)
}

// seriesStillActive is true unless RRULE has UNTIL strictly before windowStart.
func seriesStillActive(master *ical.Component, windowStart time.Time) bool {
	rrule := propValue(master.Props.Get(ical.PropRecurrenceRule))
	if rrule == "" {
		return true
	}

	for _, part := range strings.Split(rrule, ";") {
		if !strings.HasPrefix(part, "UNTIL=") {
			continue
		}
		until, ok := parseRawICSTime(strings.TrimPrefix(part, "UNTIL="))
		if ok && until.Before(windowStart) {
			return false
		}
	}

	return true
}

func exceptionInWindow(comp *ical.Component, start, end time.Time) bool {
	if t, ok := componentTime(comp, ical.PropRecurrenceID); ok {
		return inHalfOpenRange(t, start, end)
	}
	return overlapsWindow(comp, start, end)
}

func overlapsWindow(comp *ical.Component, start, end time.Time) bool {
	tStart, ok := componentTime(comp, ical.PropDateTimeStart)
	if !ok {
		// Keep unparseable components rather than silently dropping them.
		return true
	}

	tEnd, ok := componentTime(comp, ical.PropDateTimeEnd)
	if !ok {
		if dur := propValue(comp.Props.Get(ical.PropDuration)); dur != "" {
			if d, err := parseICSDuration(dur); err == nil {
				tEnd = tStart.Add(d)
				ok = true
			}
		}
	}
	if !ok {
		// Point event / all-day without DTEND: treat as [start, start+24h).
		tEnd = tStart.Add(24 * time.Hour)
	}

	return tStart.Before(end) && tEnd.After(start)
}

func inHalfOpenRange(t, start, end time.Time) bool {
	return !t.Before(start) && t.Before(end)
}

func componentTime(comp *ical.Component, name string) (time.Time, bool) {
	p := comp.Props.Get(name)
	if p == nil || p.Value == "" {
		return time.Time{}, false
	}

	t, err := p.DateTime(time.UTC)
	if err != nil {
		return parseRawICSTime(p.Value)
	}
	return t, true
}

// parseRawICSTime parses a bare DATE or DATE-TIME value (with optional Z).
func parseRawICSTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	layouts := []string{
		"20060102T150405Z",
		"20060102T150405",
		"20060102",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseICSDuration parses a subset of RFC 5545 durations used on events (e.g. PT1H, P1D).
func parseICSDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	neg := strings.HasPrefix(v, "-")
	if neg || strings.HasPrefix(v, "+") {
		v = v[1:]
	}
	if !strings.HasPrefix(v, "P") {
		return 0, errInvalidDuration
	}
	v = v[1:]

	var total time.Duration
	if i := strings.IndexByte(v, 'T'); i >= 0 {
		datePart, timePart := v[:i], v[i+1:]
		d, err := parseDurationDatePart(datePart)
		if err != nil {
			return 0, err
		}
		t, err := parseDurationTimePart(timePart)
		if err != nil {
			return 0, err
		}
		total = d + t
	} else {
		d, err := parseDurationDatePart(v)
		if err != nil {
			return 0, err
		}
		total = d
	}
	if neg {
		total = -total
	}
	return total, nil
}

type durationError string

func (e durationError) Error() string { return string(e) }

const errInvalidDuration = durationError("invalid ICS duration")

func parseDurationDatePart(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}

	var total time.Duration
	for v != "" {
		n, rest, ok := leadingInt(v)
		if !ok || rest == "" {
			return 0, errInvalidDuration
		}
		unit := rest[0]
		v = rest[1:]
		switch unit {
		case 'D':
			total += time.Duration(n) * 24 * time.Hour
		case 'W':
			total += time.Duration(n) * 7 * 24 * time.Hour
		default:
			return 0, errInvalidDuration
		}
	}
	return total, nil
}

func parseDurationTimePart(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}

	var total time.Duration
	for v != "" {
		n, rest, ok := leadingInt(v)
		if !ok || rest == "" {
			return 0, errInvalidDuration
		}
		unit := rest[0]
		v = rest[1:]
		switch unit {
		case 'H':
			total += time.Duration(n) * time.Hour
		case 'M':
			total += time.Duration(n) * time.Minute
		case 'S':
			total += time.Duration(n) * time.Second
		default:
			return 0, errInvalidDuration
		}
	}
	return total, nil
}

func leadingInt(s string) (int, string, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, s, false
	}
	n := 0
	for j := 0; j < i; j++ {
		n = n*10 + int(s[j]-'0')
	}
	return n, s[i:], true
}
