package query

import (
	"net/url"
	"testing"
	"time"

	"cuboid/internal/cache"
	"cuboid/internal/config"
	"cuboid/internal/limiter"
)

var ist, _ = time.LoadLocation("Asia/Kolkata")

func parser(t *testing.T) (*Parser, *config.Config) {
	t.Helper()
	cfg, err := config.Load("../../config/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, ist)
	return &Parser{Cfg: cfg, Loc: ist, Now: func() time.Time { return now }}, cfg
}

func TestParsesThePRDExamples(t *testing.T) {
	p, cfg := parser(t)
	orders := cfg.Sources.Get("orders")
	cases := []struct {
		ep, q string
	}{
		{EpCount, "range=last_7d"},
		{EpSummary, "from=2026-10-01&to=2026-10-08"},
		{EpTrend, "interval=day&range=last_30d"},
		{EpBreakdown, "by=status&range=last_7d"},
	}
	for _, c := range cases {
		v, _ := url.ParseQuery(c.q)
		if _, prob := p.Parse(c.ep, orders, nil, v, false); prob != nil {
			t.Errorf("%s?%s: %v", c.ep, c.q, prob)
		}
	}
	v, _ := url.ParseQuery("n=10&by=sales&range=this_week")
	if _, prob := p.Parse(EpTop, orders, orders.Top.Get("products"), v, false); prob != nil {
		t.Errorf("top products: %v", prob)
	}
}

// Abuse from the red-team list: each is a clear 400 or 422, never a slow query.
func TestRefusesBadInput(t *testing.T) {
	p, cfg := parser(t)
	orders := cfg.Sources.Get("orders")
	top := orders.Top.Get("products")
	cases := []struct {
		name, ep, q string
		tl          *config.TopList
	}{
		{"n above the bound", EpTop, "n=1000000000&by=sales&range=last_7d", top},
		{"n zero", EpTop, "n=0&by=sales&range=last_7d", top},
		{"unknown range", EpSummary, "range=forever", nil},
		{"from after to", EpSummary, "from=2026-10-08&to=2026-10-01", nil},
		{"range and from together", EpSummary, "range=last_7d&from=2026-10-01", nil},
		{"unreadable date", EpSummary, "from=yesterday&to=today", nil},
		{"unknown breakdown", EpBreakdown, "by=password&range=last_7d", nil},
		{"unknown parameter", EpSummary, "range=last_7d&tenant_id=43", nil},
		{"injection in a filter", EpSummary, "range=last_7d&status=" + url.QueryEscape("placed' OR '1'='1"), nil},
	}
	for _, c := range cases {
		v, err := url.ParseQuery(c.q)
		if err != nil {
			t.Fatal(err)
		}
		if _, prob := p.Parse(c.ep, orders, c.tl, v, false); prob == nil {
			t.Errorf("%s: accepted", c.name)
		} else if prob.Status < 400 || prob.Status >= 500 {
			t.Errorf("%s: status %d", c.name, prob.Status)
		}
	}
}

func TestSameQueryHasOneCacheKeyWhateverTheParameterOrder(t *testing.T) {
	p, cfg := parser(t)
	orders := cfg.Sources.Get("orders")
	a, _ := url.ParseQuery("by=status&range=last_7d")
	b, _ := url.ParseQuery("range=last_7d&by=status")
	ra, pa := p.Parse(EpBreakdown, orders, nil, a, false)
	rb, pb := p.Parse(EpBreakdown, orders, nil, b, false)
	if pa != nil || pb != nil {
		t.Fatal(pa, pb)
	}
	if ra.Canonical != rb.Canonical {
		t.Errorf("%q and %q", ra.Canonical, rb.Canonical)
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	f.Add("range=last_7d&status=placed&n=5&by=sales&from=2026-01-01&to=x")
	cfg, err := config.Load("../../config/metrics.yaml")
	if err != nil {
		f.Fatal(err)
	}
	p := &Parser{Cfg: cfg, Loc: ist, Now: time.Now}
	orders := cfg.Sources.Get("orders")
	f.Fuzz(func(t *testing.T, q string) {
		v, err := url.ParseQuery(q)
		if err != nil {
			return
		}
		for _, ep := range []string{EpCount, EpSummary, EpTrend, EpBreakdown} {
			p.Parse(ep, orders, nil, v, false)
		}
		p.Parse(EpTop, orders, orders.Top.Get("products"), v, false)
	})
}

// Regression: every flush recomputed every moving preset, and 1,000 dashboards
// queued behind the recomputations (p50 410 ms against 2 ms from cache).
func TestMovingPresetRefreshIsThrottled(t *testing.T) {
	s := &Service{Limits: limiter.New(limiter.Defaults)}
	moving := &Request{Range: "last_7d"}
	if !s.refreshDue(moving, "store:1", "q") {
		t.Fatal("the first refresh should start")
	}
	if s.refreshDue(moving, "store:1", "q") {
		t.Error("a second refresh inside the gap should wait")
	}
	if !s.refreshDue(moving, "store:2", "q") {
		t.Error("another store has its own gap")
	}
	if fixed := (&Request{}); !s.refreshDue(fixed, "store:1", "q") || !s.refreshDue(fixed, "store:1", "q") {
		t.Error("a fixed range refreshes by data version, not by gap")
	}
}

func TestStaleServingWindow(t *testing.T) {
	moving, fixed := &Request{Range: "last_7d"}, &Request{}
	age := func(d time.Duration) *cache.Entry { return &cache.Entry{Created: time.Now().Add(-d)} }
	if !servesStale(moving, age(10*time.Second)) {
		t.Error("a preset should be served during its refresh")
	}
	if servesStale(moving, age(presetMaxAge+time.Second)) {
		t.Error("a preset past its maximum age must not be served")
	}
	if servesStale(fixed, age(10*time.Second)) {
		t.Error("a fixed range should not be served 10 seconds stale")
	}
}
