package timeutil

import (
	"strings"
	"testing"
	"time"
)

var ist, _ = time.LoadLocation("Asia/Kolkata")

func TestTruncAndCeilUseTheLocalDay(t *testing.T) {
	at := time.Date(2026, 10, 31, 1, 30, 0, 0, ist) // still 30 October in UTC
	if got, want := Trunc(at, GrainDay, ist), time.Date(2026, 10, 31, 0, 0, 0, 0, ist); !got.Equal(want) {
		t.Errorf("day: %v, want %v", got, want)
	}
	if got, want := Trunc(at, GrainMonth, ist), time.Date(2026, 10, 1, 0, 0, 0, 0, ist); !got.Equal(want) {
		t.Errorf("month: %v, want %v", got, want)
	}
	if got, want := Ceil(at, GrainDay, ist), time.Date(2026, 11, 1, 0, 0, 0, 0, ist); !got.Equal(want) {
		t.Errorf("ceil: %v, want %v", got, want)
	}
	exact := time.Date(2026, 11, 1, 0, 0, 0, 0, ist)
	if got := Ceil(exact, GrainDay, ist); !got.Equal(exact) {
		t.Errorf("ceil of a boundary moved it: %v", got)
	}
}

func TestResolvePreset(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 4, 5, 999, ist) // a Wednesday
	secs := time.Date(2026, 10, 28, 15, 4, 5, 0, ist)
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, ist)
	cases := []struct {
		name     string
		from, to time.Time
	}{
		{"today", day, secs},
		{"yesterday", day.AddDate(0, 0, -1), day},
		{"this_week", time.Date(2026, 10, 26, 0, 0, 0, 0, ist), secs}, // Monday
		{"last_7d", secs.Add(-7 * 24 * time.Hour), secs},
		{"this_month", time.Date(2026, 10, 1, 0, 0, 0, 0, ist), secs},
		{"this_year", time.Date(2026, 1, 1, 0, 0, 0, 0, ist), secs},
	}
	for _, c := range cases {
		from, to, err := ResolvePreset(c.name, now, ist)
		if err != nil {
			t.Fatal(c.name, err)
		}
		if !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%s: %v to %v, want %v to %v", c.name, from, to, c.from, c.to)
		}
	}
	if _, _, err := ResolvePreset("next_week", now, ist); err == nil || !strings.Contains(err.Error(), "use one of") {
		t.Errorf("unknown preset should list the valid ones, got %v", err)
	}
}

func TestParseInstant(t *testing.T) {
	want := time.Date(2026, 10, 25, 14, 30, 0, 0, ist)
	for _, s := range []string{"2026-10-25T14:30:00", "2026-10-25T14:30", "2026-10-25 14:30:00", "2026-10-25T14:30:00+05:30", "2026-10-25T09:00:00Z"} {
		got, err := ParseInstant(s, ist)
		if err != nil {
			t.Errorf("%s: %v", s, err)
		} else if !got.Equal(want) {
			t.Errorf("%s: %v, want %v", s, got, want)
		}
	}
	for _, s := range []string{"", "yesterday", "2026-13-01", "25/10/2026"} {
		if _, err := ParseInstant(s, ist); err == nil {
			t.Errorf("%q should be refused", s)
		}
	}
}

func FuzzParseInstantNeverPanics(f *testing.F) {
	f.Add("2026-10-25T14:30:00+05:30")
	f.Add("0000-00-00")
	f.Fuzz(func(t *testing.T, s string) { ParseInstant(s, ist) })
}
