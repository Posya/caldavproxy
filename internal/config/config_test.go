package config

import (
	"testing"
)

func TestParseSourceList(t *testing.T) {
	got := parseSourceList(" /a/ , /b/\n/c/ ; /a/ ")
	want := []string{"/a/", "/b/", "/c/"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
	if parseSourceList("  ") != nil && len(parseSourceList("  ")) != 0 {
		t.Fatalf("empty input should yield empty list")
	}
}
