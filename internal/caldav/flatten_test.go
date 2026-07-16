package caldav

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestFlattenExpandsWeeklyRule(t *testing.T) {
	uid := "weekly@example.com"
	masterStart := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC) // Thursday
	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt(uid, "Standup", masterStart,
			withRRULE("FREQ=WEEKLY;BYDAY=TH;INTERVAL=1"),
			withEnd(masterStart.Add(30*time.Minute))),
	)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	kept, _ := flattenCalendar(cal, start, end)

	// Thursdays in July 2026: 2,9,16,23,30 → 5
	if kept != 5 {
		t.Fatalf("kept=%d, want 5 Thursday instances", kept)
	}
	for _, c := range cal.Children {
		if propValue(c.Props.Get(ical.PropRecurrenceRule)) != "" {
			t.Fatal("flattened feed must not contain RRULE")
		}
		if propValue(c.Props.Get(ical.PropRecurrenceID)) != "" {
			t.Fatal("flattened feed must not contain RECURRENCE-ID")
		}
		uid := propValue(c.Props.Get(ical.PropUID))
		if !strings.Contains(uid, "#") {
			t.Fatalf("instance UID should be unique, got %q", uid)
		}
		dt := propValue(c.Props.Get(ical.PropDateTimeStart))
		if !strings.HasSuffix(dt, "Z") {
			t.Fatalf("DTSTART should be UTC, got %q", dt)
		}
	}
}

func TestFlattenAppliesExceptionOverride(t *testing.T) {
	uid := "series@example.com"
	masterStart := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	excID := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	excStart := time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC)

	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt(uid, "Master title", masterStart,
			withRRULE("FREQ=WEEKLY;BYDAY=TH;INTERVAL=1"),
			withEnd(masterStart.Add(time.Hour))),
		eventAt(uid, "Override title", excStart,
			withRecurrenceID(excID),
			withEnd(excStart.Add(time.Hour))),
	)

	start := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	flattenCalendar(cal, start, end)

	if len(cal.Children) != 1 {
		t.Fatalf("events=%d, want 1", len(cal.Children))
	}
	sum := propValue(cal.Children[0].Props.Get(ical.PropSummary))
	if sum != "Override title" {
		t.Fatalf("summary=%q, want override", sum)
	}
	dt := propValue(cal.Children[0].Props.Get(ical.PropDateTimeStart))
	if dt != "20260716T110000Z" {
		t.Fatalf("DTSTART=%q, want moved override time", dt)
	}
}

func TestFlattenDropsOldSinglesAndStripsAttendees(t *testing.T) {
	cal := ical.NewCalendar()
	old := eventAt("old@example.com", "Old", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	neu := eventAt("new@example.com", "New", time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	att := ical.NewProp(ical.PropAttendee)
	att.Value = "mailto:someone@example.com"
	neu.Props.Add(att)
	cal.Children = append(cal.Children, old, neu)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	kept, _ := flattenCalendar(cal, start, end)
	if kept != 1 {
		t.Fatalf("kept=%d, want 1", kept)
	}
	if cal.Children[0].Props.Get(ical.PropAttendee) != nil {
		t.Fatal("ATTENDEE should be stripped from agenda events")
	}
}

func TestMergeFlatAgenda(t *testing.T) {
	uid := "weekly@example.com"
	masterStart := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC) // Friday
	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt(uid, "Review", masterStart,
			withRRULE("FREQ=WEEKLY;BYDAY=FR;INTERVAL=1"),
			withEnd(masterStart.Add(time.Hour))),
	)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	out, err := Merge([]*ical.Calendar{cal}, start, end)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if bytes.Contains(out, []byte("RRULE")) {
		t.Fatal("merged agenda must not contain RRULE")
	}
	if !bytes.Contains(out, []byte("SUMMARY:Review")) {
		t.Fatal("missing summary")
	}
	// Fridays Jul 3 and Jul 10
	if c := bytes.Count(out, []byte("BEGIN:VEVENT")); c != 2 {
		t.Fatalf("VEVENT count=%d, want 2", c)
	}
}

func withEnd(t time.Time) func(*ical.Event) {
	return func(ev *ical.Event) {
		ev.Props.SetDateTime(ical.PropDateTimeEnd, t)
	}
}

func TestParseICSDuration(t *testing.T) {
	d, err := parseICSDuration("PT1H30M")
	if err != nil || d != 90*time.Minute {
		t.Fatalf("PT1H30M = %v, %v", d, err)
	}
	d, err = parseICSDuration("P1D")
	if err != nil || d != 24*time.Hour {
		t.Fatalf("P1D = %v, %v", d, err)
	}
}
