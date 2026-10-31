// Package timeutil defines Cuboid's time semantics once: grains, bucket
// boundaries in the configured zone, range presets and request parsing.
// The planner, the engine and the verifier's SQL generator all use these
// definitions, so "a day" means the same thing everywhere.
package timeutil

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // the zone database must be available offline and on Windows
)

// Grain is the width of a rollup bucket.
type Grain int8

const (
	GrainNone  Grain = 0
	GrainHour  Grain = 1
	GrainDay   Grain = 2
	GrainMonth Grain = 3
)

// Grains lists every grain from coarsest to finest.
var Grains = []Grain{GrainMonth, GrainDay, GrainHour}

func (g Grain) String() string {
	switch g {
	case GrainHour:
		return "hour"
	case GrainDay:
		return "day"
	case GrainMonth:
		return "month"
	}
	return "none"
}

// ParseGrain accepts the names used in config and query parameters.
func ParseGrain(s string) (Grain, error) {
	switch s {
	case "hour":
		return GrainHour, nil
	case "day":
		return GrainDay, nil
	case "month":
		return GrainMonth, nil
	}
	return GrainNone, fmt.Errorf("unknown grain %q (use hour, day or month)", s)
}

// Trunc returns the start of the bucket of grain g containing t, in loc.
func Trunc(t time.Time, g Grain, loc *time.Location) time.Time {
	t = t.In(loc)
	switch g {
	case GrainHour:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
	case GrainDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	case GrainMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	}
	return t
}

// Next returns the start of the bucket after the one starting at b.
func Next(b time.Time, g Grain, loc *time.Location) time.Time {
	b = b.In(loc)
	switch g {
	case GrainHour:
		return time.Date(b.Year(), b.Month(), b.Day(), b.Hour()+1, 0, 0, 0, loc)
	case GrainDay:
		return time.Date(b.Year(), b.Month(), b.Day()+1, 0, 0, 0, 0, loc)
	case GrainMonth:
		return time.Date(b.Year(), b.Month()+1, 1, 0, 0, 0, 0, loc)
	}
	return b
}

// Ceil returns t if it is a bucket boundary, else the next boundary.
func Ceil(t time.Time, g Grain, loc *time.Location) time.Time {
	b := Trunc(t, g, loc)
	if b.Equal(t) {
		return b
	}
	return Next(b, g, loc)
}

// Presets are the named ranges the API accepts in range=.
var Presets = []string{"today", "yesterday", "this_week", "last_7d", "this_month", "last_30d", "last_90d", "this_year", "all"}

// Epoch is the start of the "all" preset: earlier than any plausible order.
var Epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// ResolvePreset turns a preset name into [from, to) in loc. now is truncated
// to whole seconds so that repeated requests within a second share a cache key.
func ResolvePreset(name string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	now = now.In(loc).Truncate(time.Second)
	day := Trunc(now, GrainDay, loc)
	switch name {
	case "today":
		return day, now, nil
	case "yesterday":
		return day.AddDate(0, 0, -1), day, nil
	case "this_week":
		wd := (int(day.Weekday()) + 6) % 7 // ISO week: Monday = 0
		return day.AddDate(0, 0, -wd), now, nil
	case "last_7d":
		return now.Add(-7 * 24 * time.Hour), now, nil
	case "this_month":
		return Trunc(now, GrainMonth, loc), now, nil
	case "last_30d":
		return now.Add(-30 * 24 * time.Hour), now, nil
	case "last_90d":
		return now.Add(-90 * 24 * time.Hour), now, nil
	case "this_year":
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc), now, nil
	case "all":
		return Epoch.In(loc), now, nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("unknown range %q (use one of %s)", name, strings.Join(Presets, ", "))
}

var naiveLayouts = []string{
	"2006-01-02",
	"2006-01-02T15:04",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05.999999999",
}

// ParseInstant parses an ISO 8601 date or date-time. Values without an offset
// are wall-clock times in loc, exactly as Postgres reads a timestamptz literal
// when the session time zone is loc.
func ParseInstant(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range naiveLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a date or date-time; use 2026-10-25 or 2026-10-25T14:30:00 (optionally with +05:30)", s)
}

// Format renders an instant the way responses echo it: RFC 3339 in loc with
// millisecond precision when needed.
func Format(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02T15:04:05.999Z07:00")
}

// Updated renders the B-1 freshness phrase for a lag.
func Updated(lag time.Duration, catchingUp bool) string {
	var s string
	switch {
	case lag < time.Second:
		s = "updated just now"
	case lag < time.Minute:
		n := int(lag / time.Second)
		s = plural(n, "second")
	case lag < time.Hour:
		s = plural(int(lag/time.Minute), "minute")
	default:
		s = plural(int(lag/time.Hour), "hour")
	}
	if catchingUp {
		s += ", catching up"
	}
	return s
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("updated 1 %s ago", unit)
	}
	return fmt.Sprintf("updated %d %ss ago", n, unit)
}
