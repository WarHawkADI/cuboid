package query

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"cuboid/internal/config"
	"cuboid/internal/planner"
	"cuboid/internal/store"
	"cuboid/internal/timeutil"
)

// An exact top-N list over a key with many values (customers, products) costs
// one group per key in the range, so it grows with the store. A rank rollup
// keeps each key's month totals, and lets the list read a few keys instead
// (ADR-011):
//
//  1. Split: a month the range covers only in part is either read row by row
//     for the part inside the range, or taken whole from its cell minus the
//     rows of the part outside, whichever holds fewer rows up to now. For
//     last_30d that is October's cell minus a day of rows, and November's cell
//     minus the rows after now: none. So a key's total is exactly the sum of
//     its cells over the months taken whole, plus its slice rows (signed).
//  2. Search: walk each month's cells in descending order, sum every key seen
//     exactly, and stop once the n-th largest total exceeds the sum of the
//     last cells seen in each month. Keys that slice rows add to form one
//     more list, by what is added. A key not yet seen has no cell above the
//     last of each month, and gains no more than the last addition, so it
//     cannot enter the list or tie with it: Fagin's threshold algorithm.
//  3. Rank as the full query does: value DESC, then store, then key.
//
// The measure must never be negative: then a key left unseen once every list
// is exhausted totals zero, which the full query leaves out too, rather than
// a negative total it would list last. The walk pays for each list entry and
// each key-month lookup before it makes them; past rankBudget, or over more
// than rankMaxMonths months whose tails add up to a high bar, ranking every
// key is no slower, and the caller does that instead.

const (
	rankBudget    = 16384
	rankMaxMonths = 3
)

// rankKey is one ranked key of one store.
type rankKey struct {
	tenant int32
	key    int64
}

type rankRow struct {
	rankKey
	v int64
}

// listRow is one cell in a month's descending list.
type listRow struct {
	rankKey
	month int
	v     int64
}

// rankSource is what the search reads from the aggregate store.
type rankSource interface {
	// sliceTotals returns each key's signed total over the slice rows.
	sliceTotals() (map[rankKey]int64, error)
	// lists returns, for each store and month taken whole, up to depth keys
	// with the largest positive cells, largest first.
	lists(depth int) ([]listRow, error)
	// cells returns the keys' totals over the months taken whole.
	cells(keys []rankKey) (map[rankKey]int64, error)
}

// BoundInfo says how a top-N list was found, for the plan in each answer.
type BoundInfo struct {
	Rollup   string `json:"rollup"`
	Months   int    `json:"months_from_cells"`
	Slices   int    `json:"slices_from_mirror"`
	Depth    int    `json:"depth"`
	KeysRead int    `json:"keys_read"`
}

// rankSlice is a time slice read row by row: sign +1 adds rows inside the
// range, -1 removes rows of a month taken whole that fall outside it.
type rankSlice struct {
	from, to time.Time
	sign     int64
}

// splitRange decides, month by month, how [from, to) is read: the months
// whose cells are taken whole, and the slices read row by row. Time after now
// holds no rows yet (or a few early ones), so it counts as empty.
func splitRange(from, to, now time.Time, loc *time.Location) (months []time.Time, slices []rankSlice) {
	upTo := func(a, b time.Time) time.Duration { // the part of [a, b) before now
		if b.After(now) {
			b = now
		}
		if !b.After(a) {
			return 0
		}
		return b.Sub(a)
	}
	for ms := timeutil.Trunc(from, timeutil.GrainMonth, loc); ms.Before(to); {
		me := timeutil.Next(ms, timeutil.GrainMonth, loc)
		lo, hi := ms, me
		if from.After(lo) {
			lo = from
		}
		if to.Before(hi) {
			hi = to
		}
		switch {
		case lo.Equal(ms) && hi.Equal(me):
			months = append(months, ms)
		case upTo(ms, lo)+upTo(hi, me) < upTo(lo, hi):
			months = append(months, ms)
			if lo.After(ms) {
				slices = append(slices, rankSlice{ms, lo, -1})
			}
			if me.After(hi) {
				slices = append(slices, rankSlice{hi, me, -1})
			}
		default:
			slices = append(slices, rankSlice{lo, hi, 1})
		}
		ms = me
	}
	return months, slices
}

// topByCells returns the n largest keys exactly, over the given number of
// months taken whole and stores. ok is false when the walk would cost more
// than rankBudget list entries and key-month lookups.
func topByCells(src rankSource, n, months, tenants int) (rows []rankRow, info BoundInfo, ok bool, err error) {
	adj, err := src.sliceTotals()
	if err != nil {
		return nil, info, false, err
	}
	spent := 0
	afford := func(cost int) bool {
		spent += cost
		return spent <= rankBudget
	}
	known := map[rankKey]int64{}
	sum := func(keys []rankKey) error {
		if len(keys) == 0 {
			return nil
		}
		cells, err := src.cells(keys)
		if err != nil {
			return err
		}
		for _, k := range keys {
			known[k] = cells[k] + adj[k]
		}
		return nil
	}
	// Rows that add can lift a key above every cell it has. The keys they lift
	// form one more list per store, by what the rows add, walked like a month.
	ups := map[int32][]rankRow{}
	for k, v := range adj {
		if v > 0 {
			ups[k.tenant] = append(ups[k.tenant], rankRow{k, v})
		}
	}
	for _, l := range ups {
		sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v })
	}
	depth := max(2*n, 16)
	for {
		if !afford(depth * months * tenants) {
			return nil, info, false, nil
		}
		list, err := src.lists(depth)
		if err != nil {
			return nil, info, false, err
		}
		var unseen []rankKey
		see := func(k rankKey) {
			if _, ok := known[k]; !ok {
				known[k] = 0 // placeholder until summed below
				unseen = append(unseen, k)
			}
		}
		type listID struct {
			tenant int32
			month  int
		}
		count, last := map[listID]int{}, map[listID]int64{}
		for _, r := range list {
			id := listID{r.tenant, r.month}
			count[id]++
			if c, ok := last[id]; !ok || r.v < c {
				last[id] = r.v
			}
			see(r.rankKey)
		}
		// A list shorter than depth is exhausted: an unseen key has no
		// positive cell in it. Otherwise its cell there is at most the last,
		// and what slice rows add to it at most the last addition read.
		bound := map[int32]int64{}
		for id, c := range count {
			if c == depth {
				bound[id.tenant] += last[id]
			}
		}
		for t, l := range ups {
			for _, r := range l[:min(depth, len(l))] {
				see(r.rankKey)
			}
			if len(l) > depth {
				bound[t] += l[depth-1].v
			}
		}
		if !afford(len(unseen) * months) {
			return nil, info, false, nil
		}
		if err := sum(unseen); err != nil {
			return nil, info, false, err
		}
		var threshold int64
		for _, b := range bound {
			threshold = max(threshold, b)
		}
		rows = rows[:0]
		for k, v := range known {
			if v != 0 {
				rows = append(rows, rankRow{k, v})
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.v != b.v {
				return a.v > b.v
			}
			if a.tenant != b.tenant {
				return a.tenant < b.tenant
			}
			return a.key < b.key
		})
		if threshold == 0 || (len(rows) >= n && rows[n-1].v > threshold) {
			if len(rows) > n {
				rows = rows[:n]
			}
			return rows, BoundInfo{Depth: depth, KeysRead: len(known)}, true, nil
		}
		depth *= 2 // each round looks up only keys it has not seen: doubling overshoots least
	}
}

// rankTop answers a pushed-down top-N list through its rank rollup, when one
// is built and the list qualifies. ok=false means: rank every key instead.
func (s *Service) rankTop(ctx context.Context, tx pgx.Tx, cfg *config.Config, plan *planner.Plan, t Target, top *topLimit) ([]raw, bool, error) {
	q := plan.Query
	m := plan.Source.Measures.Get(top.measure)
	if len(q.GroupBy) != 1 || len(q.Filters) != 0 || q.Interval != timeutil.GrainNone || m == nil || !m.NeverNegative() || !q.From.Before(q.To) {
		return nil, false, nil
	}
	r := cfg.RankRollup(plan.Source.Name, q.GroupBy[0])
	if r == nil {
		return nil, false, nil
	}
	if ready := s.Active().Planner.Ready; ready != nil && !ready(r.Name) {
		return nil, false, nil
	}
	months, slices := splitRange(q.From, q.To, s.Now(), s.Loc)
	if len(months) > rankMaxMonths {
		return nil, false, nil
	}
	src := &aggRank{ctx: ctx, tx: tx, cfg: cfg, plan: plan, rollup: r, measure: top.measure, tenants: t.Tenants, months: months, slices: slices}
	rows, info, ok, err := topByCells(src, top.n, len(months), len(t.Tenants))
	if err != nil || !ok {
		return nil, false, err
	}
	info.Rollup, info.Months, info.Slices = r.Name, len(months), len(slices)
	top.bound = &info
	// The total for shares splits the same way: the store's month totals plus
	// the slice rows already summed. Reading it as a range instead would read
	// the current hour row by row, the busiest rows a store has.
	if tr := totalsRollup(cfg, plan.Source.Name, s.Active().Planner.Ready); tr != nil {
		total, err := src.total(tr)
		if err != nil {
			return nil, false, err
		}
		top.total, top.totaled = total, true
	}
	mi := plan.Source.MeasureIndex(top.measure)
	nm := plan.Source.Measures.Len()
	out := make([]raw, len(rows))
	for i, row := range rows {
		out[i] = raw{tenant: row.tenant, keys: []int64{row.key}, vals: make([]int64, nm)}
		out[i].vals[mi] = row.v
	}
	return out, true, nil
}

// totalsRollup returns a built rollup with no dimensions and month cells for
// all time over source, or nil.
func totalsRollup(cfg *config.Config, source string, ready func(string) bool) *config.Rollup {
	for _, r := range cfg.RollupsFor(source) {
		if len(r.Dims) == 0 && r.HasGrain(timeutil.GrainMonth) && r.Retention[timeutil.GrainMonth] == 0 && (ready == nil || ready(r.Name)) {
			return r
		}
	}
	return nil
}

// aggRank reads the search from the aggregate store, in the request's
// snapshot and under its row-level security. Every statement is planned for
// its own values: a cached generic plan cannot tell a few keys from all of
// them, and turned the cells lookup into a scan of every month.
type aggRank struct {
	ctx     context.Context
	tx      pgx.Tx
	cfg     *config.Config
	plan    *planner.Plan
	rollup  *config.Rollup
	measure string
	tenants []int32
	months  []time.Time
	slices  []rankSlice
	adj     map[rankKey]int64 // the slice totals, once read
}

// total is the measure over every key: the month cells of a rollup with no
// dimensions plus the slice rows.
func (a *aggRank) total(r *config.Rollup) (int64, error) {
	var total int64
	if len(a.months) > 0 {
		err := a.tx.QueryRow(a.ctx, store.RankTotalSQL(a.plan.Source, a.measure, store.Names{}), pgx.QueryExecModeExec,
			a.tenants, r.ID, a.months).Scan(&total)
		if err != nil {
			return 0, err
		}
	}
	for _, v := range a.adj {
		total += v
	}
	return total, nil
}

func (a *aggRank) totals(sql string, args ...any) (map[rankKey]int64, error) {
	rows, err := a.tx.Query(a.ctx, sql, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[rankKey]int64{}
	for rows.Next() {
		var k rankKey
		var v int64
		if err := rows.Scan(&k.tenant, &k.key, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (a *aggRank) sliceTotals() (map[rankKey]int64, error) {
	if len(a.slices) == 0 {
		return map[rankKey]int64{}, nil
	}
	var from, to []time.Time
	var sign []int64
	for _, s := range a.slices {
		from, to, sign = append(from, s.from), append(to, s.to), append(sign, s.sign)
	}
	sql := store.RankSlicesSQL(a.cfg, a.plan.Source, a.plan.Query.GroupBy[0], a.measure, store.Names{})
	adj, err := a.totals(sql, a.tenants, from, to, sign)
	a.adj = adj
	return adj, err
}

func (a *aggRank) lists(depth int) ([]listRow, error) {
	if len(a.months) == 0 {
		return nil, nil
	}
	rows, err := a.tx.Query(a.ctx, store.RankListSQL(a.plan.Source, a.measure, store.Names{}), pgx.QueryExecModeExec, a.tenants, a.months, a.rollup.ID, depth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []listRow
	for rows.Next() {
		var r listRow
		if err := rows.Scan(&r.tenant, &r.key, &r.month, &r.v); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (a *aggRank) cells(keys []rankKey) (map[rankKey]int64, error) {
	if len(a.months) == 0 {
		return map[rankKey]int64{}, nil
	}
	ts := make([]int32, len(keys))
	ids := make([]int64, len(keys))
	for i, k := range keys {
		ts[i], ids[i] = k.tenant, k.key
	}
	args := []any{ts, ids, a.rollup.ID}
	for _, m := range a.months {
		args = append(args, m)
	}
	return a.totals(store.RankCellsSQL(a.plan.Source, a.measure, store.Names{}, len(a.months)), args...)
}
