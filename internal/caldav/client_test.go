package caldav

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

// makeCalendar builds a VCALENDAR containing one VEVENT (with the given UID)
// and, optionally, a VTIMEZONE with the given TZID.
func makeCalendar(uid, tzid string) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//test//EN")

	if tzid != "" {
		tz := ical.NewComponent(ical.CompTimezone)
		tz.Props.SetText(ical.PropTimezoneID, tzid)

		std := ical.NewComponent(ical.CompTimezoneStandard)
		std.Props.SetDateTime(ical.PropDateTimeStart, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
		std.Props.SetText(ical.PropTimezoneOffsetFrom, "+0300")
		std.Props.SetText(ical.PropTimezoneOffsetTo, "+0300")
		tz.Children = append(tz.Children, std)

		cal.Children = append(cal.Children, tz)
	}

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC))
	ev.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC))
	ev.Props.SetText(ical.PropSummary, "Event "+uid)
	cal.Children = append(cal.Children, ev.Component)

	return cal
}

func TestMergeCombinesEventsAndDedupesTimezones(t *testing.T) {
	cals := []*ical.Calendar{
		makeCalendar("a@example.com", "Europe/Moscow"),
		makeCalendar("b@example.com", "Europe/Moscow"), // same TZID -> dedup
		makeCalendar("c@example.com", "America/New_York"),
	}

	out, err := Merge(cals)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	dec := ical.NewDecoder(bytes.NewReader(out))
	merged, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode merged output: %v", err)
	}

	var events, timezones int
	for _, c := range merged.Children {
		switch c.Name {
		case ical.CompEvent:
			events++
		case ical.CompTimezone:
			timezones++
		}
	}

	if events != 3 {
		t.Errorf("events = %d, want 3", events)
	}
	if timezones != 2 {
		t.Errorf("timezones = %d, want 2 (deduped)", timezones)
	}

	if got := merged.Props.Get(ical.PropProductID); got == nil || got.Value != prodID {
		t.Errorf("PRODID = %v, want %q", got, prodID)
	}
}

func TestMergeEmptyReturnsValidEmptyCalendar(t *testing.T) {
	out, err := Merge(nil)
	if err != nil {
		t.Fatalf("Merge(nil): %v", err)
	}
	if !bytes.Contains(out, []byte("BEGIN:VCALENDAR")) || !bytes.Contains(out, []byte("END:VCALENDAR")) {
		t.Fatalf("empty calendar is not well-formed:\n%s", out)
	}
	if strings.Contains(string(out), "BEGIN:VEVENT") {
		t.Errorf("empty calendar should contain no events")
	}
}
