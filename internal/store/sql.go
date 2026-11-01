package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/planner"
	"cuboid/internal/timeutil"
)

// Names maps logical tables to physical ones, so the same SQL builds live
// tables or a rebuild's shadow copies. A rebuild from the mirror reads the
// live mirror and writes shadow cells; a rebuild from main uses shadows of both.
type Names struct {
	CellSuffix string // "" or "_shadow", a rebuild's shadow copy
}

// Cells is the cell table of a fact source.
func (n Names) Cells(source string) string {
	return QuoteIdent(config.CellTable(source) + n.CellSuffix)
}

// Mirror is the mirror of a source table.
func (n Names) Mirror(source string) string {
	return QuoteIdent(config.MirrorTable(source))
}

// joins collects the LEFT JOINs a set of dimension expressions needs.
type joins struct {
	order []string
	seen  map[string]bool
}

func (j *joins) add(alias, clause string) {
	if j.seen == nil {
		j.seen = map[string]bool{}
	}
	if !j.seen[alias] {
		j.seen[alias] = true
		j.order = append(j.order, clause)
	}
}

func (j *joins) String() string { return strings.Join(j.order, "\n") }

// dimExpr returns the SQL expression for the key id of a dimension on fact
// alias f. Text values map through the dictionary; NULL maps to 0, as the
// engine does.
func dimExpr(cfg *config.Config, src *config.Source, name string, n Names, j *joins) string {
	d := src.Dimensions.Get(name)
	tcol := QuoteIdent(cfg.Tenancy.TenantColumn)
	value := "f." + QuoteIdent(d.Column)
	if d.Join != nil {
		ja := "j_" + d.Join.Table
		j.add(ja, fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = f.%s AND %s.%s = f.%s",
			n.Mirror(d.Join.Table), ja, ja, tcol, tcol, ja, QuoteIdent(d.Join.On), QuoteIdent(d.Join.On)))
		value = ja + "." + QuoteIdent(d.Join.Column)
	}
	if d.IsKey() {
		return fmt.Sprintf("coalesce((%s)::bigint, 0)", value)
	}
	da := "d_" + name
	j.add(da, fmt.Sprintf("LEFT JOIN dict %s ON %s.dim = %s AND %s.value = (%s)::text", da, da, quoteLiteral(name), da, value))
	return fmt.Sprintf("coalesce(%s.id, 0)", da)
}

// measureRowExpr is one row's contribution to a measure, as an exact bigint.
func measureRowExpr(m *config.Measure) string {
	switch {
	case m.Type == "count":
		return "1::bigint"
	case m.MinorUnits:
		return fmt.Sprintf("coalesce(f.%s, 0)::bigint", QuoteIdent(m.Column))
	default:
		return fmt.Sprintf("coalesce(f.%s::numeric * %s, 0)::bigint", QuoteIdent(m.Column), pow10(m.Scale))
	}
}

func pow10(n int) string { return "1" + strings.Repeat("0", n) }

// Statement is one SQL statement with its arguments.
type Statement struct {
	SQL  string
	Args []any
}

// BuildRollup returns the set-based statements that compute one rollup's
// cells from the mirror, for the given tenants (nil means all). Rows go in
// primary-key order so a store's buckets sit together on disk: a range then
// reads a few thousand pages instead of one page per cell. Coarser
// grains are summed from a finer grain when that grain is kept for all time,
// so the mirror is scanned once per rollup instead of once per grain.
func BuildRollup(cfg *config.Config, r *config.Rollup, loc *time.Location, now time.Time, n Names, tenants []int32) []Statement {
	src := cfg.Sources.Get(r.Source)
	tz := quoteLiteral(loc.String())
	cells := n.Cells(src.Name)
	var measureCols, sums, sumCells []string
	for _, m := range src.Measures.Values() {
		measureCols = append(measureCols, QuoteIdent(m.Name))
		sums = append(sums, fmt.Sprintf("exact_sum(%s)", measureRowExpr(m)))
		sumCells = append(sumCells, fmt.Sprintf("exact_sum(%s)", QuoteIdent(m.Name)))
	}
	var j joins
	k := []string{"0", "0", "0"}
	for i, dim := range r.Dims {
		k[i] = dimExpr(cfg, src, dim, n, &j)
	}
	tcol := QuoteIdent(cfg.Tenancy.TenantColumn)
	tcolumn := QuoteIdent(src.Time)
	var tenantArg any
	if tenants != nil {
		tenantArg = tenants
	}
	var out []Statement
	var derivable timeutil.Grain                     // finest grain built so far that is kept for all time
	for i := len(timeutil.Grains) - 1; i >= 0; i-- { // hour, day, month
		g := timeutil.Grains[i]
		if !r.HasGrain(g) {
			continue
		}
		if r.Retention[g] == 0 && derivable != timeutil.GrainNone {
			out = append(out, Statement{SQL: fmt.Sprintf(`INSERT INTO %s (tenant_id, rollup, grain, bucket, k1, k2, k3, %s)
				SELECT tenant_id, rollup, %d, date_trunc(%s, bucket, %s), k1, k2, k3, %s
				FROM %s WHERE rollup = %d AND grain = %d AND ($1::int[] IS NULL OR tenant_id = ANY($1))
				GROUP BY tenant_id, rollup, 4, k1, k2, k3
				ORDER BY tenant_id, 4, k1, k2, k3`,
				cells, strings.Join(measureCols, ", "), g, quoteLiteral(g.String()), tz, strings.Join(sumCells, ", "),
				cells, r.ID, derivable), Args: []any{tenantArg}})
			continue
		}
		since := time.Time{}
		if r.Retention[g] > 0 {
			since = r.Since(g, now, loc)
		}
		out = append(out, Statement{SQL: fmt.Sprintf(`INSERT INTO %s (tenant_id, rollup, grain, bucket, k1, k2, k3, %s)
			SELECT f.%s, %d, %d, date_trunc(%s, f.%s, %s), %s, %s, %s, %s
			FROM %s f
			%s
			WHERE ($1::int[] IS NULL OR f.%s = ANY($1)) AND f.%s >= $2
			GROUP BY 1, 4, 5, 6, 7
			ORDER BY 1, 4, 5, 6, 7`,
			cells, strings.Join(measureCols, ", "),
			tcol, r.ID, g, quoteLiteral(g.String()), tcolumn, tz, k[0], k[1], k[2], strings.Join(sums, ", "),
			n.Mirror(src.Name), j.String(), tcol, tcolumn), Args: []any{tenantArg, since}})
		if r.Retention[g] == 0 {
			derivable = g
		}
	}
	return out
}

// SeedDictionary inserts every dimension value present in the mirror, so the
// set-based build can join to ids. Values are not tenant data.
func SeedDictionary(cfg *config.Config, n Names) []string {
	var out []string
	for _, src := range cfg.FactSources() {
		for _, d := range src.Dimensions.Values() {
			if d.IsKey() {
				continue
			}
			table, col := n.Mirror(src.Name), QuoteIdent(d.Column)
			if d.Join != nil {
				table, col = n.Mirror(d.Join.Table), QuoteIdent(d.Join.Column)
			}
			out = append(out, fmt.Sprintf(`INSERT INTO dict (dim, value)
				SELECT DISTINCT %s, (%s)::text FROM %s WHERE %s IS NOT NULL
				ON CONFLICT (dim, value) DO NOTHING`, quoteLiteral(d.Name), col, table, col))
		}
	}
	return out
}

// DictionarySource is one query, run in a source database, that lists the
// values of one text dimension found there.
type DictionarySource struct {
	Dim string
	SQL string
}

// DictionarySources lists every text dimension value of the given source
// tables as their own database holds them. A store's load seeds the shared
// dictionary from these in a short transaction of its own, before its long
// one: stores loading in parallel then never wait on each other's ids.
func DictionarySources(cfg *config.Config, sources []*config.Source) []DictionarySource {
	var out []DictionarySource
	for _, src := range sources {
		if src.Time == "" {
			continue
		}
		for _, d := range src.Dimensions.Values() {
			if d.IsKey() {
				continue
			}
			table, col := QuoteIdent(src.Name), QuoteIdent(d.Column)
			if d.Join != nil {
				table, col = QuoteIdent(d.Join.Table), QuoteIdent(d.Join.Column)
			}
			out = append(out, DictionarySource{Dim: d.Name, SQL: fmt.Sprintf("SELECT DISTINCT (%s)::text FROM %s WHERE %s IS NOT NULL", col, table, col)})
		}
	}
	return out
}

// Encoder maps engine key values (raw text) to cell key ids.
type Encoder struct {
	Cfg  *config.Config
	Dict *Dict
}

// KeyID returns the id of a value of a rollup dimension, or false when a
// text value is not in the dictionary (so no cell can hold it).
func (e Encoder) KeyID(src *config.Source, dim, value string) (int64, bool, error) {
	if value == engine.Null {
		return 0, true, nil
	}
	d := src.Dimensions.Get(dim)
	if d.IsKey() {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("%s must be an integer, got %q", dim, value)
		}
		return n, true, nil
	}
	id, ok := e.Dict.Lookup(dim, value)
	return id, ok, nil
}

// QuerySQL turns a plan into one statement against AGG: whole-bucket cells
// unioned with the edge slices from the mirror, aggregated per result key.
// It returns ok=false when a filter value does not exist (no row can match).
// Output columns: tenant_id, bucket (Unix seconds, 0 without interval),
// one key id per GroupBy dimension, then each measure.
//
// only, if set, sums that measure alone and returns 0 for the others: summing
// three measures over 100,000 groups costs twice as much as ranking by one.
func QuerySQL(pl *planner.Plan, enc Encoder, loc *time.Location, n Names, only string) (sql string, args []any, ok bool, err error) {
	q, src, r := pl.Query, pl.Source, pl.Rollup
	cfg := enc.Cfg
	tz := quoteLiteral(loc.String())
	args = []any{q.Tenants}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	var mcols, mrow, msum []string
	for _, m := range src.Measures.Values() {
		mcols = append(mcols, QuoteIdent(m.Name))
		mrow = append(mrow, measureRowExpr(m))
		if only != "" && m.Name != only {
			msum = append(msum, "0::bigint AS "+QuoteIdent(m.Name))
			continue
		}
		msum = append(msum, fmt.Sprintf("exact_sum(%s) AS %s", QuoteIdent(m.Name), QuoteIdent(m.Name)))
	}
	filterIDs := map[string]int64{}
	for dim, v := range q.Filters {
		id, found, err := enc.KeyID(src, dim, v)
		if err != nil {
			return "", nil, false, err
		}
		if !found {
			return "", nil, false, nil
		}
		filterIDs[dim] = id
	}
	var parts []string
	if len(pl.Segments) > 0 {
		var segs []string
		for _, s := range pl.Segments {
			segs = append(segs, fmt.Sprintf("(grain = %d AND bucket >= %s AND bucket < %s)", s.Grain, arg(s.From), arg(s.To)))
		}
		where := []string{"tenant_id = ANY($1)", "rollup = " + arg(r.ID), "(" + strings.Join(segs, " OR ") + ")"}
		for dim, id := range filterIDs {
			where = append(where, fmt.Sprintf("k%d = %s", pl.FilterPos[dim]+1, arg(id)))
		}
		parts = append(parts, fmt.Sprintf("SELECT tenant_id, bucket AS t, k1, k2, k3, %s FROM %s WHERE %s",
			strings.Join(mcols, ", "), n.Cells(src.Name), strings.Join(where, " AND ")))
	}
	if len(pl.Edges) > 0 {
		var j joins
		k := []string{"0", "0", "0"}
		for i, dim := range r.Dims {
			k[i] = dimExpr(cfg, src, dim, n, &j)
		}
		tcol, tcolumn := QuoteIdent(cfg.Tenancy.TenantColumn), QuoteIdent(src.Time)
		var edges []string
		for _, e := range pl.Edges {
			edges = append(edges, fmt.Sprintf("(f.%s >= %s AND f.%s < %s)", tcolumn, arg(e.From), tcolumn, arg(e.To)))
		}
		where := []string{fmt.Sprintf("f.%s = ANY($1)", tcol), "(" + strings.Join(edges, " OR ") + ")"}
		for dim, id := range filterIDs {
			where = append(where, fmt.Sprintf("%s = %s", k[pl.FilterPos[dim]], arg(id)))
		}
		var ms []string
		for i, e := range mrow {
			ms = append(ms, e+" AS "+mcols[i])
		}
		parts = append(parts, fmt.Sprintf("SELECT f.%s AS tenant_id, f.%s AS t, %s AS k1, %s AS k2, %s AS k3, %s FROM %s f\n%s\nWHERE %s",
			tcol, tcolumn, k[0], k[1], k[2], strings.Join(ms, ", "), n.Mirror(src.Name), j.String(), strings.Join(where, " AND ")))
	}
	if len(parts) == 0 {
		return "", nil, false, nil
	}
	bucket := "0::bigint"
	if q.Interval != timeutil.GrainNone {
		bucket = fmt.Sprintf("extract(epoch FROM date_trunc(%s, t, %s))::bigint", quoteLiteral(q.Interval.String()), tz)
	}
	sel := []string{"tenant_id", bucket + " AS bucket_id"}
	group := []string{"1", "2"}
	for i, pos := range pl.Pos {
		sel = append(sel, fmt.Sprintf("k%d AS key%d", pos+1, i))
		group = append(group, strconv.Itoa(3+i))
	}
	sql = fmt.Sprintf("SELECT %s, %s FROM (\n%s\n) s GROUP BY %s",
		strings.Join(sel, ", "), strings.Join(msum, ", "), strings.Join(parts, "\nUNION ALL\n"), strings.Join(group, ", "))
	return sql, args, true, nil
}

// TopSQL narrows a QuerySQL statement to the n largest groups by a measure,
// ordered value DESC, then store and key ASC, the API's top-N order. Only the
// database sees every group, so a store's busiest list costs one sort rather
// than sending every customer to the application. Key dimensions only: ids sort
// as their labels do, and a dictionary id does not sort as its text.
func TopSQL(sql string, nKeys int, measure string, n int) string {
	m := QuoteIdent(measure)
	order := []string{m + " DESC", "tenant_id"}
	for i := 0; i < nKeys; i++ {
		order = append(order, "key"+strconv.Itoa(i))
	}
	return fmt.Sprintf("SELECT * FROM (%s) g WHERE %s <> 0 ORDER BY %s LIMIT %d", sql, m, strings.Join(order, ", "), n)
}

// RankListSQL returns each month's largest cells of a rank rollup, through
// the partial index CellDDL creates: up to $4 per store in $1 and month in
// $2, with the month's position in $2 and the cell's value. $3 is the rollup.
func RankListSQL(src *config.Source, measure string, n Names) string {
	m := "c." + QuoteIdent(measure)
	return fmt.Sprintf(`SELECT x.tenant_id, x.k1, b.i - 1, x.v FROM unnest($1::int[]) AS t(id) CROSS JOIN unnest($2::timestamptz[]) WITH ORDINALITY AS b(m, i)
CROSS JOIN LATERAL (SELECT c.tenant_id, c.k1, %[2]s AS v FROM %[1]s c
	WHERE c.tenant_id = t.id AND c.rollup = $3 AND c.grain = %[3]d AND c.k2 = 0 AND c.bucket = b.m AND %[2]s > 0
	ORDER BY %[2]s DESC LIMIT $4) x`, n.Cells(src.Name), m, timeutil.GrainMonth)
}

// RankCellsSQL sums the cells of given keys over months: $1 and $2 pair
// stores with keys, $3 is the rollup, and $4 onwards are the months' buckets.
// Each key and month is one primary-key lookup, written out so no plan can do
// otherwise: as a join, the planner read every key's month cells, and with
// bucket = ANY(...) it filtered on bucket and read every month once per key
// (3.7 s for 640 keys at 10M). k2 and k3, always 0 in a rank rollup, are left
// out on purpose: with them, the partial index applies, and it reads a month.
func RankCellsSQL(src *config.Source, measure string, n Names, months int) string {
	parts := make([]string, months)
	for i := range parts {
		parts[i] = fmt.Sprintf(`(SELECT exact_sum(c.%s) FROM %s c
	WHERE c.tenant_id = q.t AND c.rollup = $3 AND c.grain = %d AND c.bucket = $%d AND c.k1 = q.k)`,
			QuoteIdent(measure), n.Cells(src.Name), timeutil.GrainMonth, 4+i)
	}
	return fmt.Sprintf("SELECT q.t, q.k, %s\nFROM unnest($1::int[], $2::bigint[]) AS q(t, k)", strings.Join(parts, "\n + "))
}

// RankTotalSQL sums the month cells of a rollup with no dimensions: $1
// stores, $2 the rollup, $3 the months' buckets. A few cells per store.
func RankTotalSQL(src *config.Source, measure string, n Names) string {
	return fmt.Sprintf(`SELECT exact_sum(c.%s) FROM %s c
WHERE c.tenant_id = ANY($1::int[]) AND c.rollup = $2 AND c.grain = %d AND c.bucket = ANY($3::timestamptz[]) AND c.k1 = 0 AND c.k2 = 0 AND c.k3 = 0`,
		QuoteIdent(measure), n.Cells(src.Name), timeutil.GrainMonth)
}

// RankSlicesSQL sums one measure per key over time slices of the mirror, each
// with its sign: $2, $3 and $4 hold the slices' starts, ends and signs, and $1
// the stores. Keys are as dimExpr gives them, so NULL is 0, as in the cells.
func RankSlicesSQL(cfg *config.Config, src *config.Source, dim, measure string, n Names) string {
	tm := "f." + QuoteIdent(src.Time)
	return fmt.Sprintf(`SELECT f.%[1]s, coalesce((f.%[2]s)::bigint, 0), exact_sum(s.sign * %[3]s)
FROM unnest($2::timestamptz[], $3::timestamptz[], $4::bigint[]) AS s(lo, hi, sign)
JOIN %[4]s f ON f.%[1]s = ANY($1::int[]) AND %[5]s >= s.lo AND %[5]s < s.hi
GROUP BY 1, 2`, QuoteIdent(cfg.Tenancy.TenantColumn), QuoteIdent(src.Dimensions.Get(dim).Column),
		measureRowExpr(src.Measures.Get(measure)), n.Mirror(src.Name), tm)
}
