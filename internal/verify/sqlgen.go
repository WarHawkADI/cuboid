// Package verify proves "the numbers match a direct database query": it runs
// the equivalent SQL on the main database and compares exactly, while writes
// continue, by fencing the replication stream at a WAL position.
package verify

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/planner"
	"cuboid/internal/store"
	"cuboid/internal/timeutil"
)

// DirectSQL builds the direct query on the main database from the same
// semantic definitions the API uses (internal/timeutil, Section 9.2):
// created_at >= from AND created_at < to, filters as equality, join
// dimensions through their current dimension row, buckets in the zone.
// Output: tenant_id, bucket (Unix seconds or 0), group values as text,
// then every measure as an exact bigint.
func DirectSQL(cfg *config.Config, q *planner.Query, loc *time.Location) (string, []any) {
	src := cfg.Sources.Get(q.Source)
	tcol := store.QuoteIdent(cfg.Tenancy.TenantColumn)
	args := []any{q.Tenants, q.From, q.To}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	joins := map[string]string{}
	var joinOrder []string
	valueExpr := func(dim string) string {
		d := src.Dimensions.Get(dim)
		if d.Join == nil {
			return "o." + store.QuoteIdent(d.Column)
		}
		alias := "j_" + d.Join.Table
		if _, ok := joins[alias]; !ok {
			joins[alias] = fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = o.%s AND %s.%s = o.%s", store.QuoteIdent(d.Join.Table), alias,
				alias, tcol, tcol, alias, store.QuoteIdent(d.Join.On), store.QuoteIdent(d.Join.On))
			joinOrder = append(joinOrder, alias)
		}
		return alias + "." + store.QuoteIdent(d.Join.Column)
	}
	sel := []string{"o." + tcol}
	group := []string{"1", "2"}
	if q.Interval != timeutil.GrainNone {
		sel = append(sel, fmt.Sprintf("extract(epoch FROM date_trunc('%s', o.%s, '%s'))::bigint", q.Interval, store.QuoteIdent(src.Time), loc.String()))
	} else {
		sel = append(sel, "0::bigint")
	}
	for i, g := range q.GroupBy {
		sel = append(sel, valueExpr(g)+"::text")
		group = append(group, strconv.Itoa(3+i))
	}
	for _, m := range src.Measures.Values() {
		switch {
		case m.Type == "count":
			sel = append(sel, "count(*)")
		case m.MinorUnits:
			sel = append(sel, fmt.Sprintf("coalesce(sum(o.%s), 0)::bigint", store.QuoteIdent(m.Column)))
		default:
			sel = append(sel, fmt.Sprintf("coalesce(sum(o.%s) * 1%s, 0)::bigint", store.QuoteIdent(m.Column), strings.Repeat("0", m.Scale)))
		}
	}
	where := []string{fmt.Sprintf("o.%s = ANY($1)", tcol), fmt.Sprintf("o.%s >= $2", store.QuoteIdent(src.Time)), fmt.Sprintf("o.%s < $3", store.QuoteIdent(src.Time))}
	for _, dim := range sortedKeys(q.Filters) {
		d := src.Dimensions.Get(dim)
		if d.IsKey() {
			n, _ := strconv.ParseInt(q.Filters[dim], 10, 64)
			where = append(where, fmt.Sprintf("%s = %s", valueExpr(dim), arg(n)))
		} else {
			where = append(where, fmt.Sprintf("%s = %s", valueExpr(dim), arg(q.Filters[dim])))
		}
	}
	var js []string
	for _, a := range joinOrder {
		js = append(js, joins[a])
	}
	sql := fmt.Sprintf("SELECT %s\nFROM %s o\n%s\nWHERE %s\nGROUP BY %s",
		strings.Join(sel, ", "), store.QuoteIdent(src.Name), strings.Join(js, "\n"), strings.Join(where, "\n  AND "), strings.Join(group, ", "))
	return strings.ReplaceAll(sql, "\n\n", "\n"), args
}

// Printable renders SQL with its arguments inlined, for people to paste into
// psql. It is never executed; DirectSQL's bound form is.
func Printable(sql string, args []any, loc *time.Location) string {
	for i := len(args); i >= 1; i-- {
		var lit string
		switch v := args[i-1].(type) {
		case []int32:
			parts := make([]string, len(v))
			for j, t := range v {
				parts[j] = strconv.Itoa(int(t))
			}
			lit = "ARRAY[" + strings.Join(parts, ",") + "]"
		case time.Time:
			lit = "'" + v.In(loc).Format("2006-01-02 15:04:05.999999-07:00") + "'"
		case string:
			lit = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		default:
			lit = fmt.Sprint(v)
		}
		sql = strings.ReplaceAll(sql, "$"+strconv.Itoa(i), lit)
	}
	return sql
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
