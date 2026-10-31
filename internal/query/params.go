package query

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/planner"
	"cuboid/internal/store"
	"cuboid/internal/timeutil"
)

// Endpoint names.
const (
	EpCount     = "count"
	EpSummary   = "summary"
	EpTrend     = "trend"
	EpBreakdown = "breakdown"
	EpTop       = "top"
)

// Request is a parsed, validated dashboard query.
type Request struct {
	Endpoint  string
	Source    *config.Source
	Top       *config.TopList // for EpTop
	Query     planner.Query
	Range     string // preset name, or "" for explicit from/to
	N         int
	Measure   string // trend metric, top "by", breakdown metric
	BreakDim  string // breakdown "by" (or "store" for companies)
	SplitBy   string // trend split
	ByStore   bool
	Echo      map[string]any
	Canonical string
}

// Problem is an RFC 7807-style error in Cuboid's plain voice.
type Problem struct {
	Status int    `json:"status"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (p *Problem) Error() string { return p.Title + ": " + p.Detail }

func bad(detail, hint string) *Problem {
	return &Problem{Status: 400, Type: "invalid_parameter", Title: "Invalid query", Detail: detail, Hint: hint}
}

// Parser validates query parameters against the active configuration.
type Parser struct {
	Cfg  *config.Config
	Loc  *time.Location
	Now  func() time.Time
	Dict *store.Dict
}

var common = map[string]bool{"from": true, "to": true, "range": true}

// Parse builds a Request for endpoint ep on source src (or top list tl).
func (p *Parser) Parse(ep string, src *config.Source, tl *config.TopList, v url.Values, company bool) (*Request, *Problem) {
	allowed := map[string]bool{}
	for k := range common {
		allowed[k] = true
	}
	for _, d := range src.Dimensions.Keys() {
		allowed[d] = true
	}
	switch ep {
	case EpTrend:
		allowed["interval"], allowed["metric"], allowed["split_by"] = true, true, true
	case EpTop:
		allowed["n"], allowed["by"] = true, true
	case EpBreakdown:
		allowed["by"], allowed["metric"] = true, true
	}
	for k, vals := range v {
		if !allowed[k] {
			keys := make([]string, 0, len(allowed))
			for a := range allowed {
				keys = append(keys, a)
			}
			sort.Strings(keys)
			return nil, bad(fmt.Sprintf("unknown parameter %q", k), "accepted parameters here: "+strings.Join(keys, ", "))
		}
		if len(vals) != 1 {
			return nil, bad(fmt.Sprintf("parameter %q is given %d times", k, len(vals)), "give each parameter once")
		}
	}
	req := &Request{Endpoint: ep, Source: src, Top: tl, Echo: map[string]any{"endpoint": endpointName(src, tl, ep)}}
	req.Query = planner.Query{Source: src.Name, Filters: map[string]string{}}
	if prob := p.parseRange(req, v); prob != nil {
		return nil, prob
	}
	for _, d := range src.Dimensions.Values() {
		val := v.Get(d.Name)
		if val == "" {
			continue
		}
		if prob := p.checkValue(d, val); prob != nil {
			return nil, prob
		}
		req.Query.Filters[d.Name] = val
		req.Echo[d.Name] = val
	}
	measure := func(name, def string) (string, *Problem) {
		m := v.Get(name)
		if m == "" {
			m = def
		}
		if src.Measures.Get(m) == nil {
			return "", bad(fmt.Sprintf("%s=%q is not a measure", name, m), "use one of "+strings.Join(src.Measures.Keys(), ", "))
		}
		req.Echo[name] = m
		return m, nil
	}
	var prob *Problem
	switch ep {
	case EpTrend:
		g := timeutil.GrainDay
		if s := v.Get("interval"); s != "" {
			var err error
			if g, err = timeutil.ParseGrain(s); err != nil {
				return nil, bad(err.Error(), "interval=hour, day or month")
			}
		}
		req.Query.Interval = g
		req.Echo["interval"] = g.String()
		if req.Measure, prob = measure("metric", src.CountMeasure); prob != nil {
			return nil, prob
		}
		if s := v.Get("split_by"); s != "" {
			if src.Dimensions.Get(s) == nil {
				return nil, bad(fmt.Sprintf("split_by=%q is not a dimension", s), "use one of "+strings.Join(src.Dimensions.Keys(), ", "))
			}
			req.SplitBy = s
			req.Query.GroupBy = []string{s}
			req.Echo["split_by"] = s
		}
	case EpTop:
		req.N = 10
		if s := v.Get("n"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 100 {
				return nil, bad(fmt.Sprintf("n=%q is out of range", s), "n must be a whole number from 1 to 100")
			}
			req.N = n
		}
		req.Echo["n"] = req.N
		def := src.CountMeasure
		for _, m := range src.Measures.Values() {
			if m.Type == "sum" {
				def = m.Name
				break
			}
		}
		if req.Measure, prob = measure("by", def); prob != nil {
			return nil, prob
		}
		req.Query.GroupBy = []string{tl.Dim}
	case EpBreakdown:
		by := v.Get("by")
		if by == "" {
			return nil, bad("by is required", "for example by=status")
		}
		if by == "store" && company {
			req.ByStore = true
		} else if src.Dimensions.Get(by) == nil {
			hint := "use one of " + strings.Join(src.Dimensions.Keys(), ", ")
			if company {
				hint += ", or store"
			}
			return nil, bad(fmt.Sprintf("by=%q is not a dimension", by), hint)
		} else {
			req.Query.GroupBy = []string{by}
		}
		req.BreakDim = by
		req.Echo["by"] = by
		if req.Measure, prob = measure("metric", src.CountMeasure); prob != nil {
			return nil, prob
		}
	case EpCount:
		req.Measure = src.CountMeasure
	}
	req.Canonical = canonical(req)
	return req, nil
}

func endpointName(src *config.Source, tl *config.TopList, ep string) string {
	if tl != nil {
		return tl.Name + "/top"
	}
	return src.Name + "/" + ep
}

func (p *Parser) parseRange(req *Request, v url.Values) *Problem {
	rng, from, to := v.Get("range"), v.Get("from"), v.Get("to")
	now := p.Now()
	switch {
	case rng != "" && (from != "" || to != ""):
		return bad("give either range or from/to, not both", "for example range=last_7d, or from=2026-10-01&to=2026-10-08")
	case rng != "":
		f, t, err := timeutil.ResolvePreset(rng, now, p.Loc)
		if err != nil {
			return bad(err.Error(), "")
		}
		req.Range = rng
		req.Query.From, req.Query.To = f, t
		req.Echo["range"] = rng
	case from != "" || to != "":
		if from == "" || to == "" {
			return bad("from and to go together", "for example from=2026-10-01&to=2026-10-08 (to is exclusive)")
		}
		f, err := timeutil.ParseInstant(from, p.Loc)
		if err != nil {
			return bad(err.Error(), "")
		}
		t, err := timeutil.ParseInstant(to, p.Loc)
		if err != nil {
			return bad(err.Error(), "")
		}
		if !f.Before(t) {
			return bad("from must be before to", "to is exclusive: from=2026-10-01&to=2026-10-02 is one day")
		}
		if t.Sub(f) > 30*366*24*time.Hour {
			return bad("the range is longer than 30 years", "use range=all for everything")
		}
		req.Query.From, req.Query.To = f, t
		req.Echo["from"], req.Echo["to"] = from, to
	default:
		f, t, _ := timeutil.ResolvePreset("last_7d", now, p.Loc)
		req.Range = "last_7d"
		req.Query.From, req.Query.To = f, t
		req.Echo["range"] = "last_7d"
	}
	return nil
}

// filterValue is what a text filter may contain. Values are checked by shape
// only, never against the global dictionary: answering "unknown value" for a
// category another store uses would tell this store that it exists. An
// unknown value simply matches no rows, exactly as the direct SQL would.
var filterValue = regexp.MustCompile(`^[\p{L}\p{N} _.,&()/+-]{1,100}$`)

// checkValue validates a filter value. Nothing unvalidated reaches SQL, and
// values are always bound as parameters.
func (p *Parser) checkValue(d *config.Dimension, val string) *Problem {
	if d.IsKey() {
		if n, err := strconv.ParseInt(val, 10, 64); err != nil || n <= 0 {
			return bad(fmt.Sprintf("%s=%q is not an id", d.Name, val), d.Name+" takes a positive whole number")
		}
		return nil
	}
	if !filterValue.MatchString(val) {
		return bad(fmt.Sprintf("%s=%q contains characters no %s value has", d.Name, val, d.Name), "GET /tenants/{id}/meta lists this store's values for each dimension")
	}
	return nil
}

func canonical(r *Request) string {
	keys := make([]string, 0, len(r.Echo))
	for k := range r.Echo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v;", k, r.Echo[k])
	}
	fmt.Fprintf(&b, "[%d,%d)", r.Query.From.UnixMilli(), r.Query.To.UnixMilli())
	return b.String()
}
