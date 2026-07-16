package caldav

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/emersion/go-ical"
)

func TestFoldICSShortLineUnchanged(t *testing.T) {
	in := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n")
	out := foldICS(in)
	if !bytes.Equal(out, in) {
		t.Fatalf("short document changed:\n got %q\nwant %q", out, in)
	}
}

func TestFoldICSLongLine(t *testing.T) {
	long := "ATTENDEE;CN=Someone;PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT:mailto:" +
		strings.Repeat("a", 80) + "@example.com"
	in := []byte("BEGIN:VEVENT\r\n" + long + "\r\nEND:VEVENT\r\n")
	out := foldICS(in)

	for _, line := range bytes.Split(out, []byte("\r\n")) {
		if len(line) == 0 {
			continue
		}
		if len(line) > maxContentOctets {
			t.Fatalf("line exceeds %d octets (%d): %q", maxContentOctets, len(line), line)
		}
	}

	unfolded := string(unfoldICS(out))
	if !strings.Contains(unfolded, long) {
		t.Fatalf("folded output does not round-trip to original line:\n%s", unfolded)
	}
}

func TestFoldICSDoesNotSplitUTF8(t *testing.T) {
	// Build a line that would land a fold inside a multi-byte rune if we split by octets naively.
	prefix := "SUMMARY:"
	// Cyrillic letters are 2 bytes in UTF-8.
	body := strings.Repeat("я", 50) // 100 octets of payload
	line := prefix + body
	in := []byte(line + "\r\n")
	out := foldICS(in)

	for _, part := range bytes.Split(out, []byte("\r\n")) {
		if len(part) == 0 {
			continue
		}
		if !utf8.Valid(part) {
			t.Fatalf("folded segment is not valid UTF-8: %q", part)
		}
		if len(part) > maxContentOctets {
			t.Fatalf("segment too long: %d", len(part))
		}
	}

	got := string(bytes.TrimSuffix(unfoldICS(out), []byte("\r\n")))
	if got != line {
		t.Fatalf("UTF-8 round-trip failed:\n got %q\nwant %q", got, line)
	}
}

func TestUnfoldThenFoldIdempotent(t *testing.T) {
	long := "DESCRIPTION:" + strings.Repeat("x", 200)
	in := foldICS([]byte(long + "\r\n"))
	again := foldICS(in)
	if !bytes.Equal(in, again) {
		t.Fatalf("foldICS is not idempotent")
	}
}

func TestMergeFoldsLongLines(t *testing.T) {
	cal := makeCalendar("long@example.com", "")
	att := ical.Prop{Name: ical.PropAttendee}
	att.Value = "mailto:" + strings.Repeat("u", 80) + "@example.com"
	att.Params = ical.Params{
		"CN":   []string{"Very Long Attendee Name Here"},
		"ROLE": []string{"REQ-PARTICIPANT"},
	}
	cal.Children[0].Props.Add(&att)

	out, err := Merge([]*ical.Calendar{cal})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	for _, line := range bytes.Split(out, []byte("\r\n")) {
		if len(line) == 0 {
			continue
		}
		if len(line) > maxContentOctets {
			t.Fatalf("merged feed has unfolded line (%d): %q", len(line), line)
		}
	}
}
