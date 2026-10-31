// Package reference is a deliberately simple, obviously correct
// implementation of the dashboard queries: every row in memory, every query
// answered by brute force. It is used only by tests, as the oracle the
// engine, planner and mirror edges are checked against.
package reference

import (
	"strconv"
	"strings"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/planner"
	"cuboid/internal/timeutil"
)

// DB holds the current rows of every source table.
type DB struct {
	cfg    *config.Config
	loc    *time.Location
	Tables map[string]map[string]engine.Row // table -> primary key -> row
}

// New returns an empty reference database.
func New(cfg *config.Config, loc *time.Location) *DB {
	return &DB{cfg: cfg, loc: loc, Tables: map[string]map[string]engine.Row{}}
}

func (db *DB) key(table string, r engine.Row) string {
	src := db.cfg.Sources.Get(table)
	parts := make([]string, len(src.Key))
	for i, k := range src.Key {
		parts[i] = r[k]
	}
	return strings.Join(parts, "|")
}

// Apply performs a change, as the main database would.
func (db *DB) Apply(c engine.Change) {
	t := db.Tables[c.Table]
	if t == nil {
		t = map[string]engine.Row{}
		db.Tables[c.Table] = t
	}
	if c.Old != nil {
		delete(t, db.key(c.Table, c.Old))
	}
	if c.New != nil {
		t[db.key(c.Table, c.New)] = c.New
	}
}

// Get returns the current row with the given primary key values.
func (db *DB) Get(table string, keyVals ...string) engine.Row {
	return db.Tables[table][strings.Join(keyVals, "|")]
}

// Answer computes q by scanning every row: the definition of the right answer.
func (db *DB) Answer(q *planner.Query) planner.Result {
	src := db.cfg.Sources.Get(q.Source)
	out := planner.Result{}
	tcol := db.cfg.Tenancy.TenantColumn
	for _, row := range db.Tables[q.Source] {
		t64, _ := strconv.ParseInt(row[tcol], 10, 32)
		tenant := int32(t64)
		if !contains(q.Tenants, tenant) {
			continue
		}
		ts, err := engine.ParseTimestamp(row[src.Time], db.loc)
		if err != nil || ts.Before(q.From) || !ts.Before(q.To) {
			continue
		}
		dims := map[string]string{}
		for _, d := range src.Dimensions.Values() {
			dims[d.Name] = db.dimValue(d, row)
		}
		match := true
		for d, v := range q.Filters {
			if dims[d] != v {
				match = false
			}
		}
		if !match {
			continue
		}
		k := planner.ResultKey{Tenant: tenant}
		if q.Interval != timeutil.GrainNone {
			k.Bucket = timeutil.Trunc(ts, q.Interval, db.loc).Unix()
		}
		for i, d := range q.GroupBy {
			k.K[i] = dims[d]
		}
		v := make([]int64, src.Measures.Len())
		for i, m := range src.Measures.Values() {
			v[i], _ = engine.MeasureValue(m, row)
		}
		out.Add(k, v, 1)
	}
	return out.DropZeros()
}

// dimValue resolves a dimension as a SQL LEFT JOIN against current rows would.
func (db *DB) dimValue(d *config.Dimension, row engine.Row) string {
	if d.Join == nil {
		if v, ok := row[d.Column]; ok {
			return v
		}
		return engine.Null
	}
	on, ok := row[d.Join.On]
	if !ok {
		return engine.Null
	}
	dim := db.Get(d.Join.Table, row[db.cfg.Tenancy.TenantColumn], on)
	if dim == nil {
		return engine.Null
	}
	if v, ok := dim[d.Join.Column]; ok {
		return v
	}
	return engine.Null
}

func contains(ts []int32, t int32) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}
