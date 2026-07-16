package caldav

import (
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

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
