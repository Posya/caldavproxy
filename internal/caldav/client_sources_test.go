package caldav

import (
	"testing"

	"github.com/emersion/go-ical"
)

func TestResolveCalendarURL(t *testing.T) {
	t.Parallel()

	got, err := resolveCalendarURL("https://caldav.example.com/", "/calendars/u/work/")
	if err != nil {
		t.Fatalf("relative: %v", err)
	}
	if want := "https://caldav.example.com/calendars/u/work/"; got.String() != want {
		t.Fatalf("relative: got %q want %q", got.String(), want)
	}

	abs := "https://other.example.com/dav/cal/"
	got, err = resolveCalendarURL("https://caldav.example.com/", abs)
	if err != nil {
		t.Fatalf("absolute: %v", err)
	}
	if got.String() != abs {
		t.Fatalf("absolute: got %q want %q", got.String(), abs)
	}

	if _, err := resolveCalendarURL("https://caldav.example.com/", "  "); err == nil {
		t.Fatal("empty path should error")
	}
}

func TestPrefixUIDs(t *testing.T) {
	t.Parallel()

	cal := ical.NewCalendar()
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "same@example.com")
	cal.Children = append(cal.Children, ev.Component)

	prefixUIDs([]*ical.Calendar{cal}, "s0")
	if got := propValue(ev.Props.Get(ical.PropUID)); got != "s0|same@example.com" {
		t.Fatalf("got uid %q", got)
	}

	// Idempotent for the same prefix.
	prefixUIDs([]*ical.Calendar{cal}, "s0")
	if got := propValue(ev.Props.Get(ical.PropUID)); got != "s0|same@example.com" {
		t.Fatalf("double prefix: got uid %q", got)
	}
}
