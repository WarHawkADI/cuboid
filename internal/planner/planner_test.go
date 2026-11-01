package planner

import (
	"errors"
	"sort"
	"testing"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/timeutil"
)

var ist, _ = time.LoadLocation("Asia/Kolkata")

func testPlanner(t *testing.T) *Planner {
	t.Helper()
	cfg, err := config.Load("../../config/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, ist)
	return &Planner{Cfg: cfg, Loc: ist, Now: func() time.Time { return now }, Ready: func(string) bool { return true }}
}

func q(from, to time.Time, groupBy ...string) *Query {
	return &Query{Source: "orders", Tenants: []int32{1}, From: from, To: to, GroupBy: groupBy}
}

func TestPicksTheSmallestCoveringRollup(t *testing.T) {
	p := testPlanner(t)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, ist)
	to := time.Date(2026, 10, 8, 0, 0, 0, 0, ist)
	cases := []struct {
		groupBy []string
		want    string
	}{{nil, "totals"}, {[]string{"status"}, "by_status"}, {[]string{"category"}, "status_category"}, {[]string{"customer_id"}, "top_customer"}}
	for _, c := range cases {
		pl, err := p.Plan(q(from, to, c.groupBy...))
		if err != nil {
			t.Fatal(c.groupBy, err)
		}
		if pl.Rollup.Name != c.want {
			t.Errorf("%v: rollup %s, want %s", c.groupBy, pl.Rollup.Name, c.want)
		}
	}
}

// A rank rollup holds only month cells: answering from it would read any
// partial month row by row. Even for whole months it is not a plan.
func TestRankRollupsNeverAnswer(t *testing.T) {
	p := testPlanner(t)
	for _, r := range [][2]time.Time{
		{time.Date(2026, 8, 1, 0, 0, 0, 0, ist), time.Date(2026, 10, 1, 0, 0, 0, 0, ist)},
		{time.Date(2026, 10, 2, 0, 23, 28, 0, ist), time.Date(2026, 11, 1, 0, 23, 28, 0, ist)},
	} {
		pl, err := p.Plan(q(r[0], r[1], "customer_id"))
		if err != nil {
			t.Fatal(err)
		}
		if pl.Rollup.Rank {
			t.Errorf("%v: planned with rank rollup %s", r, pl.Rollup.Name)
		}
	}
}

func TestWholeDaysNeedNoEdges(t *testing.T) {
	p := testPlanner(t)
	pl, err := p.Plan(q(time.Date(2026, 10, 1, 0, 0, 0, 0, ist), time.Date(2026, 10, 8, 0, 0, 0, 0, ist)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Edges) != 0 {
		t.Errorf("whole days should read no mirror rows, got edges %v", pl.Edges)
	}
}

// The plan's pieces must count every instant of the range once and every
// instant outside it not at all: cells and edges add, minus edges subtract.
func TestPiecesCountTheRangeOnce(t *testing.T) {
	p := testPlanner(t) // now is 31 October 12:00
	for _, r := range [][2]time.Time{
		{time.Date(2026, 8, 14, 9, 17, 3, 0, ist), time.Date(2026, 10, 3, 18, 41, 59, 0, ist)},
		{time.Date(2026, 10, 24, 11, 44, 36, 0, ist), time.Date(2026, 10, 31, 11, 44, 36, 0, ist)},
		{time.Date(2026, 10, 31, 11, 5, 0, 0, ist), time.Date(2026, 10, 31, 11, 50, 0, 0, ist)},
		{time.Date(2026, 10, 31, 11, 50, 0, 0, ist), time.Date(2026, 10, 31, 12, 40, 0, 0, ist)},
		{time.Date(2026, 10, 31, 12, 10, 0, 0, ist), time.Date(2026, 10, 31, 12, 40, 0, 0, ist)},
	} {
		pl, err := p.Plan(q(r[0], r[1]))
		if err != nil {
			t.Fatal(err)
		}
		checkCount(t, pl, r[0], r[1])
		for _, e := range pl.Edges {
			if e.To.Sub(e.From) >= time.Hour {
				t.Errorf("%v: edge %v is wider than an hour", r, e)
			}
		}
	}
}

func checkCount(t *testing.T, pl *Plan, from, to time.Time) {
	t.Helper()
	cuts := []time.Time{from.Add(-2 * time.Hour), from, to, to.Add(2 * time.Hour)}
	for _, s := range pl.Segments {
		cuts = append(cuts, s.From, s.To)
	}
	for _, e := range pl.Edges {
		cuts = append(cuts, e.From, e.To)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	for i := 0; i+1 < len(cuts); i++ {
		if !cuts[i].Before(cuts[i+1]) {
			continue
		}
		at := cuts[i].Add(cuts[i+1].Sub(cuts[i]) / 2)
		n := 0
		for _, s := range pl.Segments {
			if !at.Before(s.From) && at.Before(s.To) {
				n++
			}
		}
		for _, e := range pl.Edges {
			if !at.Before(e.From) && at.Before(e.To) {
				if e.Minus {
					n--
				} else {
					n++
				}
			}
		}
		want := 0
		if !at.Before(from) && at.Before(to) {
			want = 1
		}
		if n != want {
			t.Fatalf("[%v, %v): %v is counted %d times, want %d (segments %v, edges %v)", from, to, at, n, want, pl.Segments, pl.Edges)
		}
	}
}

// At 12:44:36, the last 7 days end in the current hour. Its cell less the rows
// dated after now replaces reading every row since 12:00. A week ago, the
// range starts 15 minutes before 13:00: those rows are fewer than the 44
// minutes the cell would need subtracted, so they are still read.
func TestCurrentHourIsItsCellLessTheFuture(t *testing.T) {
	p := testPlanner(t)
	now := time.Date(2026, 10, 31, 12, 44, 36, 0, ist)
	p.Now = func() time.Time { return now }
	from, to := now.AddDate(0, 0, -7), now
	pl, err := p.Plan(q(from, to))
	if err != nil {
		t.Fatal(err)
	}
	checkCount(t, pl, from, to)
	hour := time.Date(2026, 10, 31, 12, 0, 0, 0, ist)
	var minus, lead bool
	for _, e := range pl.Edges {
		switch {
		case e.Minus && e.From.Equal(now) && e.To.Equal(hour.Add(time.Hour)):
			minus = true
		case !e.Minus && e.From.Equal(from) && e.To.Equal(time.Date(2026, 10, 24, 13, 0, 0, 0, ist)):
			lead = true
		case !e.Minus && e.To.After(hour):
			t.Errorf("the current hour is read row by row: %v", e)
		}
	}
	if !minus || !lead {
		t.Errorf("want the rows after now subtracted and the week-old 15 minutes read, got edges %v", pl.Edges)
	}
	if d := pl.EdgeDuration(); d != 15*time.Minute+24*time.Second {
		t.Errorf("edge time before now %v, want 15m24s", d)
	}
}

func TestUncoveredQueryIsRefusedWithAHint(t *testing.T) {
	p := testPlanner(t)
	_, err := p.Plan(q(time.Date(2026, 10, 1, 0, 0, 0, 0, ist), time.Date(2026, 10, 8, 0, 0, 0, 0, ist), "payment_method", "customer_id"))
	var np *NotPreaggregated
	if !errors.As(err, &np) || np.Hint == "" {
		t.Fatalf("want a NotPreaggregated with a hint, got %v", err)
	}
}

func TestUnbuiltRollupIsNotUsed(t *testing.T) {
	p := testPlanner(t)
	p.Ready = func(name string) bool { return name != "by_status" }
	pl, err := p.Plan(q(time.Date(2026, 10, 1, 0, 0, 0, 0, ist), time.Date(2026, 10, 8, 0, 0, 0, 0, ist), "status"))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Rollup.Name == "by_status" {
		t.Error("a rollup still building was chosen")
	}
}

func TestBucketsCountWholeGrains(t *testing.T) {
	p := testPlanner(t)
	pl, err := p.Plan(q(time.Date(2026, 9, 1, 0, 0, 0, 0, ist), time.Date(2026, 10, 3, 0, 0, 0, 0, ist)))
	if err != nil {
		t.Fatal(err)
	}
	b := pl.Buckets(ist)
	if b[timeutil.GrainMonth] != 1 || b[timeutil.GrainDay] != 2 {
		t.Errorf("buckets %v, want one month and two days", b)
	}
}
