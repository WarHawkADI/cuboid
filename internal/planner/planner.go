// Package planner answers "which pre-computed cells add up to this query?".
//
// It picks the smallest rollup whose dimensions cover the query (the view
// selection idea from Harinarayan, Rajaraman and Ullman, "Implementing Data
// Cubes Efficiently", 1996), then splits [from, to) greedily into whole
// months, days and hours that rollup holds, plus at most a few thin edge
// slices that are read row by row from the mirror. The edges are what make
// arbitrary timestamps exact.
package planner

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/timeutil"
)

// Query is a typed dashboard query, already authorised and validated.
type Query struct {
	Source   string
	Tenants  []int32
	From, To time.Time
	Filters  map[string]string // dimension -> value
	GroupBy  []string          // dimensions to group by (top-N key, breakdown dimension)
	Interval timeutil.Grain    // trend bucket width; GrainNone for totals
}

// Segment is a run of whole buckets of one grain read from rollup cells.
type Segment struct {
	Grain    timeutil.Grain
	From, To time.Time
}

// Edge is a time slice read row by row from the mirror.
type Edge struct{ From, To time.Time }

// Plan says how a query is assembled.
type Plan struct {
	Query    *Query
	Source   *config.Source
	Rollup   *config.Rollup
	Segments []Segment
	Edges    []Edge
	// Pos maps each GroupBy dimension to its key position in the rollup.
	Pos []int
	// FilterPos maps filtered dimensions to key positions.
	FilterPos map[string]int
}

// NotPreaggregated is returned when no rollup covers a query. Cuboid never
// falls back to the main database; it says what to add instead.
type NotPreaggregated struct{ Hint string }

func (e *NotPreaggregated) Error() string { return e.Hint }

// Planner builds plans against the active configuration.
type Planner struct {
	Cfg *config.Config
	Loc *time.Location
	Now func() time.Time
	// Ready reports whether a rollup is fully built (new rollups added by a
	// config reload are not used until their build has cut over).
	Ready func(rollup string) bool
}

// Plan returns the cheapest plan for q.
func (p *Planner) Plan(q *Query) (*Plan, error) {
	src := p.Cfg.Sources.Get(q.Source)
	if src == nil || src.Time == "" {
		return nil, fmt.Errorf("unknown source %q", q.Source)
	}
	if !q.From.Before(q.To) {
		return &Plan{Query: q, Source: src, Rollup: nil}, nil
	}
	need := map[string]bool{}
	for _, d := range q.GroupBy {
		need[d] = true
	}
	for d := range q.Filters {
		need[d] = true
	}
	var cands []*config.Rollup
	for _, r := range p.Cfg.RollupsFor(src.Name) {
		if p.Ready != nil && !p.Ready(r.Name) {
			continue
		}
		// A rank rollup only bounds top-N lists: with month cells alone, any
		// range would be read row by row from the mirror.
		if r.Rank {
			continue
		}
		ok := true
		for d := range need {
			if r.DimIndex(d) < 0 {
				ok = false
				break
			}
		}
		if ok {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return nil, &NotPreaggregated{Hint: p.hint(src, q, need)}
	}
	var best *Plan
	var bestCost float64
	for _, r := range cands {
		pl := p.planWith(q, src, r)
		c := pl.relativeCost(src)
		if best == nil || c < bestCost {
			best, bestCost = pl, c
		}
	}
	return best, nil
}

func (p *Planner) planWith(q *Query, src *config.Source, r *config.Rollup) *Plan {
	pl := &Plan{Query: q, Source: src, Rollup: r, FilterPos: map[string]int{}}
	for _, d := range q.GroupBy {
		pl.Pos = append(pl.Pos, r.DimIndex(d))
	}
	for d := range q.Filters {
		pl.FilterPos[d] = r.DimIndex(d)
	}
	now := p.Now()
	var avail []grainAvail
	for _, g := range timeutil.Grains {
		if !r.HasGrain(g) {
			continue
		}
		if q.Interval != timeutil.GrainNone && g > q.Interval {
			continue // a month cell cannot be split into days
		}
		var since time.Time
		if r.Retention[g] > 0 {
			// One day of margin: the boundary moves with the clock, and a
			// bucket is only used once every row of it is certainly held.
			since = timeutil.Ceil(r.Since(g, now, p.Loc).Add(24*time.Hour), g, p.Loc)
		}
		avail = append(avail, grainAvail{g: g, since: since})
	}
	p.decompose(pl, q.From, q.To, avail)
	pl.Edges = mergeEdges(pl.Edges)
	return pl
}

type grainAvail struct {
	g     timeutil.Grain
	since time.Time // zero: all time
}

func (p *Planner) decompose(pl *Plan, from, to time.Time, avail []grainAvail) {
	if !from.Before(to) {
		return
	}
	if len(avail) == 0 {
		pl.Edges = append(pl.Edges, Edge{From: from, To: to})
		return
	}
	g := avail[0]
	lo := from
	if !g.since.IsZero() && lo.Before(g.since) {
		lo = g.since
	}
	a := timeutil.Ceil(lo, g.g, p.Loc)
	b := timeutil.Trunc(to, g.g, p.Loc)
	if a.Before(b) {
		pl.Segments = append(pl.Segments, Segment{Grain: g.g, From: a, To: b})
		p.decompose(pl, from, a, avail[1:])
		p.decompose(pl, b, to, avail[1:])
		return
	}
	p.decompose(pl, from, to, avail[1:])
}

func mergeEdges(es []Edge) []Edge {
	if len(es) < 2 {
		return es
	}
	sort.Slice(es, func(i, j int) bool { return es[i].From.Before(es[j].From) })
	out := []Edge{es[0]}
	for _, e := range es[1:] {
		last := &out[len(out)-1]
		if !e.From.After(last.To) {
			if e.To.After(last.To) {
				last.To = e.To
			}
			continue
		}
		out = append(out, e)
	}
	return out
}

// Buckets returns how many buckets each segment spans.
func (pl *Plan) Buckets(loc *time.Location) map[timeutil.Grain]int {
	out := map[timeutil.Grain]int{}
	for _, s := range pl.Segments {
		n := 0
		for b := s.From; b.Before(s.To); b = timeutil.Next(b, s.Grain, loc) {
			n++
		}
		out[s.Grain] += n
	}
	return out
}

// EdgeDuration is the total time read row by row from the mirror.
func (pl *Plan) EdgeDuration() time.Duration {
	var d time.Duration
	for _, e := range pl.Edges {
		d += e.To.Sub(e.From)
	}
	return d
}

// relativeCost ranks candidate plans without statistics: key dimensions
// (product, customer) multiply cells, and edge time costs rows.
func (pl *Plan) relativeCost(src *config.Source) float64 {
	width := 1.0
	for _, d := range pl.Rollup.Dims {
		dim := src.Dimensions.Get(d)
		switch {
		case dim.IsKey():
			width *= 400
		default:
			width *= 8
		}
	}
	cost := 0.0
	for _, s := range pl.Segments {
		hours := s.To.Sub(s.From).Hours()
		per := map[timeutil.Grain]float64{timeutil.GrainHour: 1, timeutil.GrainDay: 24, timeutil.GrainMonth: 720}[s.Grain]
		cost += hours / per * width
	}
	cost += pl.EdgeDuration().Hours() * 50 // assume ~50 rows per hour of edge
	return cost
}

func (p *Planner) hint(src *config.Source, q *Query, need map[string]bool) string {
	dims := make([]string, 0, len(need))
	for d := range need {
		dims = append(dims, d)
	}
	sort.Strings(dims)
	what := "this query"
	if len(q.GroupBy) > 0 {
		what = "grouping by " + strings.Join(q.GroupBy, ", ")
	}
	if len(q.Filters) > 0 {
		var fs []string
		for d := range q.Filters {
			fs = append(fs, d)
		}
		sort.Strings(fs)
		what += " with a filter on " + strings.Join(fs, ", ")
	}
	return fmt.Sprintf("No rollup covers %s. Add one with dims [%s] for source %s in config/metrics.yaml.", what, strings.Join(dims, ", "), src.Name)
}
