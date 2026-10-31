package planner

import (
	"errors"
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

// The plan's pieces must cover the range exactly once, with no gap or overlap.
func TestSegmentsAndEdgesTileTheRange(t *testing.T) {
	p := testPlanner(t)
	from := time.Date(2026, 8, 14, 9, 17, 3, 0, ist)
	to := time.Date(2026, 10, 3, 18, 41, 59, 0, ist)
	pl, err := p.Plan(q(from, to))
	if err != nil {
		t.Fatal(err)
	}
	type piece struct{ from, to time.Time }
	var ps []piece
	for _, s := range pl.Segments {
		ps = append(ps, piece{s.From, s.To})
	}
	for _, e := range pl.Edges {
		ps = append(ps, piece{e.From, e.To})
	}
	at := from
	for at.Before(to) {
		found := false
		for _, x := range ps {
			if x.from.Equal(at) {
				at, found = x.to, true
				break
			}
		}
		if !found {
			t.Fatalf("no piece starts at %v: %v", at, ps)
		}
	}
	if !at.Equal(to) {
		t.Errorf("pieces end at %v, want %v", at, to)
	}
	if len(pl.Edges) != 2 {
		t.Errorf("expected two thin edges, got %v", pl.Edges)
	}
	for _, e := range pl.Edges {
		if e.To.Sub(e.From) >= time.Hour {
			t.Errorf("edge %v is wider than an hour", e)
		}
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
