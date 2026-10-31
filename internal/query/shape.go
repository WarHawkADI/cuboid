package query

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"cuboid/internal/config"
	"cuboid/internal/decimal"
	"cuboid/internal/engine"
	"cuboid/internal/planner"
	"cuboid/internal/store"
	"cuboid/internal/timeutil"
)

// Response is the envelope every data endpoint returns.
type Response struct {
	TenantID  *int32         `json:"tenant_id,omitempty"`
	CompanyID *int32         `json:"company_id,omitempty"`
	Stores    []int32        `json:"stores,omitempty"`
	Query     map[string]any `json:"query"`
	From      string         `json:"from"`
	To        string         `json:"to"`
	Data      any            `json:"data"`
	Freshness Freshness      `json:"freshness"`
	Meta      Meta           `json:"meta"`
	// tenants are the stores the result rows came from. The response guard
	// checks them against the key's scope before anything is written.
	tenants []int32
	state   store.Freshness // the store state this answer was read at
}

// Tenants returns the stores the data came from.
func (r *Response) Tenants() []int32 { return r.tenants }

// Freshness is bonus B-1.
type Freshness struct {
	AsOf    string `json:"as_of,omitempty"`
	LSN     string `json:"lsn"`
	LagMS   int64  `json:"lag_ms"`
	Updated string `json:"updated"`
	Stale   bool   `json:"stale"`
}

// Meta explains how the answer was produced.
type Meta struct {
	Plan   PlanInfo `json:"plan"`
	Cache  string   `json:"cache"`
	Exact  bool     `json:"exact"`
	Heavy  bool     `json:"heavy"`
	Timing Timing   `json:"timing_ms"`
}

// Timing is wall time in milliseconds.
type Timing struct {
	Total float64 `json:"total"`
	Store float64 `json:"store"`
}

// PlanInfo is the plan, for the "where this number comes from" drawer.
type PlanInfo struct {
	Rollup        string     `json:"rollup,omitempty"`
	Segments      []SegInfo  `json:"segments"`
	Edges         []EdgeInfo `json:"edges"`
	CellsEstimate int64      `json:"cells_estimate"`
	EdgeRowsEst   int64      `json:"edge_rows_estimate"`
	// TopBound is set when a rank rollup narrowed a top-N list: the cells and
	// edges above were then not read (ADR-011).
	TopBound *BoundInfo `json:"top_bound,omitempty"`
}

// SegInfo is a run of whole buckets read from rollup cells.
type SegInfo struct {
	Grain   string `json:"grain"`
	From    string `json:"from"`
	To      string `json:"to"`
	Buckets int    `json:"buckets"`
}

// EdgeInfo is a slice read row by row from the mirror.
type EdgeInfo struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func describePlan(pl *planner.Plan, loc *time.Location, cells, edgeRows float64) PlanInfo {
	info := PlanInfo{Segments: []SegInfo{}, Edges: []EdgeInfo{}, CellsEstimate: int64(cells), EdgeRowsEst: int64(edgeRows)}
	if pl.Rollup == nil {
		return info
	}
	info.Rollup = pl.Rollup.Name
	segs := append([]planner.Segment(nil), pl.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].From.Before(segs[j].From) })
	for _, s := range segs {
		n := 0
		for b := s.From; b.Before(s.To); b = timeutil.Next(b, s.Grain, loc) {
			n++
		}
		info.Segments = append(info.Segments, SegInfo{Grain: s.Grain.String(), From: timeutil.Format(s.From, loc), To: timeutil.Format(s.To, loc), Buckets: n})
	}
	for _, e := range pl.Edges {
		info.Edges = append(info.Edges, EdgeInfo{From: timeutil.Format(e.From, loc), To: timeutil.Format(e.To, loc)})
	}
	return info
}

func (r *Response) clone() *Response {
	c := *r
	return &c
}

func (r *Response) withCache(status string) *Response {
	c := r.clone()
	c.Meta.Cache = status
	return c
}

// render returns a measure value for JSON: an exact decimal string for
// scaled measures (money), an integer otherwise. Never a float.
func render(m *config.Measure, v int64) any {
	if m.Scale > 0 {
		return decimal.Format(v, m.Scale)
	}
	return v
}

// Item is one row of a breakdown or top-N list.
type Item struct {
	Rank     int     `json:"rank,omitempty"`
	TenantID int32   `json:"tenant_id,omitempty"`
	Key      any     `json:"key"`
	Label    string  `json:"label"`
	Value    any     `json:"value"`
	Share    float64 `json:"share"`
	raw      int64
}

// Point is one trend bucket.
type Point struct {
	T     string `json:"t"`
	Value any    `json:"value"`
}

// Series is one line of a split trend.
type Series struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Points []Point `json:"points"`
}

func (s *Service) shape(ctx context.Context, tx pgx.Tx, cfg *config.Config, req *Request, pl *planner.Plan, res planner.Result, t Target, top *topLimit) (any, error) {
	src := req.Source
	mi := src.MeasureIndex(req.Measure)
	switch req.Endpoint {
	case EpCount:
		var n int64
		for _, v := range res {
			n += v[mi]
		}
		return map[string]any{"count": n}, nil
	case EpSummary:
		tot := make([]int64, src.Measures.Len())
		for _, v := range res {
			for i := range v {
				tot[i] += v[i]
			}
		}
		out := map[string]any{}
		for i, m := range src.Measures.Values() {
			out[m.Name] = render(m, tot[i])
		}
		for _, d := range src.Derived.Values() {
			num, den := src.Measures.Get(d.Num), src.Measures.Get(d.Den)
			q := decimal.DivHalfUp(tot[src.MeasureIndex(d.Num)]*pow10(den.Scale), tot[src.MeasureIndex(d.Den)])
			out[d.Name] = render(num, q)
		}
		return out, nil
	case EpTrend:
		return s.shapeTrend(req, res, mi), nil
	case EpBreakdown:
		if req.ByStore {
			return s.shapeByStore(req, res, mi, t), nil
		}
		items, err := s.items(ctx, tx, cfg, req, res, mi, req.BreakDim, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"by": req.BreakDim, "metric": req.Measure, "items": items}, nil
	case EpTop:
		items, err := s.items(ctx, tx, cfg, req, res, mi, req.Top.Dim, top)
		if err != nil {
			return nil, err
		}
		if len(items) > req.N {
			items = items[:req.N]
		}
		for i := range items {
			items[i].Rank = i + 1
		}
		return map[string]any{"by": req.Measure, "dimension": req.Top.Dim, "items": items}, nil
	}
	return nil, fmt.Errorf("unknown endpoint %q", req.Endpoint)
}

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// items groups a result by one dimension. Key dimensions (product, customer)
// stay separate per store, because product 5 of one store is not product 5
// of another; text dimensions (status, category) are summed across stores.
func (s *Service) items(ctx context.Context, tx pgx.Tx, cfg *config.Config, req *Request, res planner.Result, mi int, dimName string, top *topLimit) ([]Item, error) {
	src := req.Source
	dim := src.Dimensions.Get(dimName)
	m := src.Measures.Values()[mi]
	type key struct {
		tenant int32
		val    string
	}
	sums := map[key]int64{}
	var total int64
	for k, v := range res {
		kk := key{val: k.K[0]}
		if dim.IsKey() {
			kk.tenant = k.Tenant
		}
		sums[kk] += v[mi]
		total += v[mi]
	}
	if top != nil {
		total = top.total // the rows are only the largest groups; the total is of them all
	}
	items := make([]Item, 0, len(sums))
	for k, v := range sums {
		if v == 0 {
			continue
		}
		it := Item{raw: v, Value: render(m, v)}
		if k.val == engine.Null {
			it.Key, it.Label = nil, "(none)"
		} else if dim.IsKey() {
			id, _ := strconv.ParseInt(k.val, 10, 64)
			it.Key, it.Label = id, k.val
		} else {
			it.Key, it.Label = k.val, k.val
		}
		if dim.IsKey() && len(req.Query.Tenants) > 1 {
			it.TenantID = k.tenant
		}
		if total != 0 {
			it.Share = float64(v) / float64(total)
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].raw != items[j].raw {
			return items[i].raw > items[j].raw
		}
		return lessKey(items[i], items[j])
	})
	if dim.IsKey() && dim.Label != nil {
		limit := len(items)
		if req.Endpoint == EpTop && req.N < limit {
			limit = req.N
		}
		if err := s.label(ctx, tx, cfg, dim, req.Query.Tenants, res, items[:limit]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func lessKey(a, b Item) bool {
	if a.TenantID != b.TenantID {
		return a.TenantID < b.TenantID
	}
	switch x := a.Key.(type) {
	case int64:
		y, _ := b.Key.(int64)
		return x < y
	case string:
		y, _ := b.Key.(string)
		return x < y
	}
	return b.Key != nil
}

// label reads names for key dimensions from the mirror, inside the same
// tenant-scoped transaction as the numbers.
func (s *Service) label(ctx context.Context, tx pgx.Tx, cfg *config.Config, dim *config.Dimension, tenants []int32, res planner.Result, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	tenantOf := map[int64]int32{}
	for k := range res {
		if id, err := strconv.ParseInt(k.K[0], 10, 64); err == nil {
			tenantOf[id] = k.Tenant
		}
	}
	var ts []int32
	var ids []int64
	for _, it := range items {
		id, ok := it.Key.(int64)
		if !ok {
			continue
		}
		t := it.TenantID
		if t == 0 {
			t = tenantOf[id]
		}
		ts = append(ts, t)
		ids = append(ids, id)
	}
	tcol := store.QuoteIdent(cfg.Tenancy.TenantColumn)
	keyCol := store.QuoteIdent(dim.Column)
	sql := fmt.Sprintf(`SELECT m.%s, m.%s::bigint, m.%s::text FROM %s m
		JOIN unnest($1::int[], $2::bigint[]) AS u(t, id) ON m.%s = u.t AND m.%s = u.id`,
		tcol, keyCol, store.QuoteIdent(dim.Label.Column), store.Names{}.Mirror(dim.Label.Table), tcol, keyCol)
	rows, err := tx.Query(ctx, sql, ts, ids)
	if err != nil {
		return fmt.Errorf("read labels: %w", err)
	}
	defer rows.Close()
	type lk struct {
		t  int32
		id int64
	}
	names := map[lk]string{}
	for rows.Next() {
		var t int32
		var id int64
		var name string
		if err := rows.Scan(&t, &id, &name); err != nil {
			return err
		}
		if !contains(tenants, t) {
			return &errCrossTenant{got: t, want: tenants}
		}
		names[lk{t, id}] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		id, _ := items[i].Key.(int64)
		t := items[i].TenantID
		if t == 0 {
			t = tenantOf[id]
		}
		if n, ok := names[lk{t, id}]; ok {
			items[i].Label = n
		}
	}
	return nil
}

func (s *Service) shapeTrend(req *Request, res planner.Result, mi int) any {
	src := req.Source
	m := src.Measures.Values()[mi]
	g := req.Query.Interval
	var buckets []time.Time
	for b := timeutil.Trunc(req.Query.From, g, s.Loc); b.Before(req.Query.To); b = timeutil.Next(b, g, s.Loc) {
		buckets = append(buckets, b)
	}
	if req.SplitBy == "" {
		sum := map[int64]int64{}
		for k, v := range res {
			sum[k.Bucket] += v[mi]
		}
		pts := make([]Point, len(buckets))
		for i, b := range buckets {
			pts[i] = Point{T: timeutil.Format(b, s.Loc), Value: render(m, sum[b.Unix()])}
		}
		return map[string]any{"interval": g.String(), "metric": req.Measure, "points": pts}
	}
	// Split: the five largest values of the split dimension, the rest as "other".
	byKey := map[string]map[int64]int64{}
	totals := map[string]int64{}
	for k, v := range res {
		if byKey[k.K[0]] == nil {
			byKey[k.K[0]] = map[int64]int64{}
		}
		byKey[k.K[0]][k.Bucket] += v[mi]
		totals[k.K[0]] += v[mi]
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	other := map[int64]int64{}
	var series []Series
	for i, k := range keys {
		if i >= 5 {
			for b, v := range byKey[k] {
				other[b] += v
			}
			continue
		}
		label := k
		if k == engine.Null {
			label = "(none)"
		}
		sr := Series{Key: k, Label: label}
		for _, b := range buckets {
			sr.Points = append(sr.Points, Point{T: timeutil.Format(b, s.Loc), Value: render(m, byKey[k][b.Unix()])})
		}
		series = append(series, sr)
	}
	if len(keys) > 5 {
		sr := Series{Key: "other", Label: "Other"}
		for _, b := range buckets {
			sr.Points = append(sr.Points, Point{T: timeutil.Format(b, s.Loc), Value: render(m, other[b.Unix()])})
		}
		series = append(series, sr)
	}
	return map[string]any{"interval": g.String(), "metric": req.Measure, "split_by": req.SplitBy, "series": series}
}

func (s *Service) shapeByStore(req *Request, res planner.Result, mi int, t Target) any {
	m := req.Source.Measures.Values()[mi]
	sums := map[int32]int64{}
	var total int64
	for k, v := range res {
		sums[k.Tenant] += v[mi]
		total += v[mi]
	}
	var items []Item
	for _, tenant := range t.Tenants {
		v := sums[tenant]
		it := Item{TenantID: tenant, Key: tenant, Label: s.Members.Name(tenant), Value: render(m, v), raw: v}
		if total != 0 {
			it.Share = float64(v) / float64(total)
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].raw != items[j].raw {
			return items[i].raw > items[j].raw
		}
		return items[i].TenantID < items[j].TenantID
	})
	return map[string]any{"by": "store", "metric": req.Measure, "items": items}
}
