package caldav

import (
	"bytes"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func eventAt(uid, summary string, start time.Time, opts ...func(*ical.Event)) *ical.Component {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
	ev.Props.SetText(ical.PropSummary, summary)
	for _, opt := range opts {
		opt(ev)
	}
	return ev.Component
}

func withRRULE(rule string) func(*ical.Event) {
	return func(ev *ical.Event) {
		ev.Props.SetText(ical.PropRecurrenceRule, rule)
	}
}

func withRecurrenceID(t time.Time) func(*ical.Event) {
	return func(ev *ical.Event) {
		ev.Props.SetDateTime(ical.PropRecurrenceID, t)
	}
}

func TestPruneDropsOldStandaloneEvents(t *testing.T) {
	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt("old@example.com", "Old", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)),
		eventAt("new@example.com", "New", time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)),
	)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	kept, dropped := pruneCalendar(cal, start, end)

	if kept != 1 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 1/1", kept, dropped)
	}
	if got := propValue(cal.Children[0].Props.Get(ical.PropUID)); got != "new@example.com" {
		t.Fatalf("remaining UID = %q", got)
	}
}

func TestPruneKeepsMasterAndInWindowExceptionsOnly(t *testing.T) {
	uid := "series@example.com"
	masterStart := time.Date(2024, 2, 1, 10, 0, 0, 0, time.UTC)
	oldExc := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	newExc := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)

	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt(uid, "Master", masterStart, withRRULE("FREQ=WEEKLY;BYDAY=TH;INTERVAL=1")),
		eventAt(uid, "Old override", oldExc, withRecurrenceID(oldExc)),
		eventAt(uid, "New override", newExc, withRecurrenceID(newExc)),
	)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	kept, dropped := pruneCalendar(cal, start, end)

	if kept != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 2/1 (master + new exception)", kept, dropped)
	}

	var summaries []string
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			summaries = append(summaries, propValue(c.Props.Get(ical.PropSummary)))
		}
	}
	if len(summaries) != 2 || summaries[0] != "Master" || summaries[1] != "New override" {
		t.Fatalf("summaries = %v", summaries)
	}
}

func TestPruneDropsExpiredSeriesWithoutInWindowExceptions(t *testing.T) {
	uid := "ended@example.com"
	masterStart := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt(uid, "Ended", masterStart, withRRULE("FREQ=WEEKLY;BYDAY=MO;UNTIL=20250101T000000Z")),
	)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	kept, dropped := pruneCalendar(cal, start, end)

	if kept != 0 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 0/1", kept, dropped)
	}
}

func TestMergePrunesRecurrenceHistory(t *testing.T) {
	uid := "series@example.com"
	masterStart := time.Date(2024, 2, 1, 10, 0, 0, 0, time.UTC)
	oldExc := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	newExc := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)

	cal := ical.NewCalendar()
	cal.Children = append(cal.Children,
		eventAt(uid, "Master", masterStart, withRRULE("FREQ=WEEKLY;BYDAY=TH;INTERVAL=1")),
		eventAt(uid, "Old", oldExc, withRecurrenceID(oldExc)),
		eventAt(uid, "New", newExc, withRecurrenceID(newExc)),
	)

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	out, err := Merge([]*ical.Calendar{cal}, start, end)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	dec := ical.NewDecoder(bytes.NewReader(out))
	merged, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := len(merged.Events()); n != 2 {
		t.Fatalf("events = %d, want 2", n)
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
