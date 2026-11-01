// Package engine turns source changes into exact rollup deltas.
//
// Every change is expressed as contributions: an INSERT adds the new row's
// contribution, a DELETE retracts the old row's, and an UPDATE retracts the
// old and adds the new. Because counts and sums are retractable, the rollups
// stay exact under any mix of changes. The package is pure: it reads no
// database, so the model-based tests drive exactly the code production runs.
package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"cuboid/internal/config"
	"cuboid/internal/decimal"
	"cuboid/internal/timeutil"
)

// Null stands for a SQL NULL dimension value. It is stored as key id 0.
const Null = "\x00"

// Row is a source row as text values keyed by column. A missing column is NULL.
type Row map[string]string

// Kind is the type of a row change.
type Kind uint8

const (
	Insert Kind = iota + 1
	Update
	Delete
)

func (k Kind) String() string {
	return [...]string{"", "insert", "update", "delete"}[k]
}

// Change is one row change from the replication stream.
type Change struct {
	Table string
	Kind  Kind
	Old   Row // set for Update and Delete (REPLICA IDENTITY FULL on fact tables)
	New   Row // set for Insert and Update
}

// Txn is one committed source transaction.
type Txn struct {
	Source     string // the source database it was committed in
	Xid        uint32
	CommitLSN  uint64
	CommitTime time.Time
	Changes    []Change
}

// Fact is a fact row resolved into what the rollups need.
type Fact struct {
	Tenant   int32
	Time     time.Time
	Dims     map[string]string // dimension name -> value (Null for NULL)
	Measures []int64           // in the source's measure order
}

// SecurityEvent is something the engine saw that an operator must know about.
type SecurityEvent struct {
	Kind     string
	Severity string
	Tenant   int32
	Detail   string
}

// DimKey identifies a dimension row: the tenant and its non-tenant key value.
type DimKey struct {
	Table  string
	Tenant int32
	Key    string
}

// DimLoader fetches dimension rows the engine has not seen yet, from the
// mirror in the aggregate store. Missing keys are simply absent from the map.
type DimLoader func(keys []DimKey) (map[DimKey]Row, error)

// Engine applies transactions to a Delta.
type Engine struct {
	cfg    *config.Config
	loc    *time.Location
	now    func() time.Time
	load   DimLoader
	dims   map[DimKey]Row
	joinBy map[string]map[string]bool // dimension table -> columns that join dimensions read
}

// New returns an engine for cfg. load may be nil (unknown dimension rows are
// treated as absent, which is what a LEFT JOIN on a missing row returns).
func New(cfg *config.Config, loc *time.Location, now func() time.Time, load DimLoader) *Engine {
	e := &Engine{cfg: cfg, loc: loc, now: now, load: load, dims: map[DimKey]Row{}}
	e.SetConfig(cfg)
	return e
}

// SetConfig switches the engine to a new configuration (B-4 live reload).
// Callers switch at a flush boundary so every transaction uses one config.
func (e *Engine) SetConfig(cfg *config.Config) {
	e.cfg = cfg
	e.joinBy = map[string]map[string]bool{}
	for _, s := range cfg.FactSources() {
		for _, d := range s.Dimensions.Values() {
			if d.Join != nil {
				if e.joinBy[d.Join.Table] == nil {
					e.joinBy[d.Join.Table] = map[string]bool{}
				}
				e.joinBy[d.Join.Table][d.Join.Column] = true
			}
		}
	}
}

// Config returns the active configuration.
func (e *Engine) Config() *config.Config { return e.cfg }

// Location returns the bucketing time zone.
func (e *Engine) Location() *time.Location { return e.loc }

// Apply adds the effect of one committed transaction to d.
func (e *Engine) Apply(txn *Txn, d *Delta) error {
	if err := e.prefetch(txn); err != nil {
		return err
	}
	// Dimension rows first, so an order inserted in the same transaction as
	// its product resolves the product's category.
	for i := range txn.Changes {
		c := &txn.Changes[i]
		src := e.cfg.Sources.Get(c.Table)
		if src == nil || src.Time != "" {
			continue
		}
		if err := e.applyDimension(src, c, d); err != nil {
			return err
		}
	}
	for i := range txn.Changes {
		c := &txn.Changes[i]
		src := e.cfg.Sources.Get(c.Table)
		if src == nil || src.Time == "" {
			continue
		}
		if err := e.applyFact(src, c, d); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) applyFact(src *config.Source, c *Change, d *Delta) error {
	var oldF, newF *Fact
	var err error
	if c.Kind == Update || c.Kind == Delete {
		if oldF, err = e.Resolve(src, c.Old); err != nil {
			return fmt.Errorf("%s %s (old row): %w", c.Table, c.Kind, err)
		}
	}
	if c.Kind == Insert || c.Kind == Update {
		if newF, err = e.Resolve(src, c.New); err != nil {
			return fmt.Errorf("%s %s (new row): %w", c.Table, c.Kind, err)
		}
	}
	if oldF != nil {
		e.contribute(src, oldF, -1, d)
	}
	if newF != nil {
		e.contribute(src, newF, +1, d)
		if newF.Time.After(e.now().Add(10 * time.Minute)) {
			d.FutureTimestamps++
		}
	}
	if oldF != nil && newF != nil && oldF.Tenant != newF.Tenant {
		d.Events = append(d.Events, SecurityEvent{
			Kind: "tenant_reassignment", Severity: "P2", Tenant: newF.Tenant,
			Detail: fmt.Sprintf("a %s row moved from store %d to store %d; both stores' numbers were corrected", src.Name, oldF.Tenant, newF.Tenant),
		})
	}
	e.mirror(src, c, d)
	return nil
}

func (e *Engine) applyDimension(src *config.Source, c *Change, d *Delta) error {
	tcol := e.cfg.Tenancy.TenantColumn
	joinCols := e.joinBy[src.Name]
	var oldKey, newKey DimKey
	var err error
	if c.Old != nil {
		if oldKey, err = e.dimKey(src, c.Old); err != nil {
			return err
		}
	}
	if c.New != nil {
		if newKey, err = e.dimKey(src, c.New); err != nil {
			return err
		}
	}
	switch c.Kind {
	case Insert:
		e.dims[newKey] = stripPII(src, c.New)
	case Update:
		if c.Old == nil { // key unchanged and no old image sent
			oldKey = newKey
		}
		prev := c.Old
		if prev == nil {
			prev = e.dims[oldKey]
		}
		delete(e.dims, oldKey)
		e.dims[newKey] = stripPII(src, c.New)
		// Without the previous row we cannot prove the join columns kept
		// their values, so assume they changed: a rebuild is always exact.
		changed := oldKey != newKey || prev == nil
		for col := range joinCols {
			if prev != nil && prev[col] != c.New[col] {
				changed = true
			}
		}
		if changed && len(joinCols) > 0 {
			d.markDimRebuild(oldKey.Tenant, src.Name)
			d.markDimRebuild(newKey.Tenant, src.Name)
		}
	case Delete:
		delete(e.dims, oldKey)
		if len(joinCols) > 0 {
			d.markDimRebuild(oldKey.Tenant, src.Name)
		}
	}
	// Labels and company membership can change any cached answer of the tenant.
	for _, k := range []DimKey{oldKey, newKey} {
		if k.Table != "" {
			d.touchAll(k.Tenant)
		}
	}
	if c.Old != nil && c.New != nil && c.Old[tcol] != c.New[tcol] {
		d.Events = append(d.Events, SecurityEvent{
			Kind: "tenant_reassignment", Severity: "P2", Tenant: newKey.Tenant,
			Detail: fmt.Sprintf("a %s row moved from store %d to store %d", src.Name, oldKey.Tenant, newKey.Tenant),
		})
	}
	e.mirror(src, c, d)
	return nil
}

// dimKey builds the DimKey of a dimension-table row from its key columns.
func (e *Engine) dimKey(src *config.Source, row Row) (DimKey, error) {
	tcol := e.cfg.Tenancy.TenantColumn
	t, err := parseTenant(row[tcol])
	if err != nil {
		return DimKey{}, fmt.Errorf("%s: %w", src.Name, err)
	}
	var parts []string
	for _, k := range src.Key {
		if k != tcol {
			parts = append(parts, row[k])
		}
	}
	return DimKey{Table: src.Name, Tenant: t, Key: strings.Join(parts, "|")}, nil
}

func (e *Engine) mirror(src *config.Source, c *Change, d *Delta) {
	key := func(r Row) (int32, string) {
		t, _ := parseTenant(r[e.cfg.Tenancy.TenantColumn])
		parts := make([]string, len(src.Key))
		for i, k := range src.Key {
			parts[i] = r[k]
		}
		return t, strings.Join(parts, "|")
	}
	if c.Old != nil {
		t, k := key(c.Old)
		if c.New == nil {
			d.mirrorOp(src.Name, k, &MirrorOp{Tenant: t, Row: nil, Old: c.Old})
		} else if nt, nk := key(c.New); nk != k || nt != t {
			d.mirrorOp(src.Name, k, &MirrorOp{Tenant: t, Row: nil, Old: c.Old})
		}
	}
	if c.New != nil {
		t, k := key(c.New)
		d.mirrorOp(src.Name, k, &MirrorOp{Tenant: t, Row: stripPII(src, c.New)})
	}
}

func stripPII(src *config.Source, r Row) Row {
	if len(src.PIIColumns) == 0 {
		return r
	}
	out := make(Row, len(r))
	for k, v := range r {
		if !src.IsPII(k) {
			out[k] = v
		}
	}
	return out
}

// Resolve reads a fact row into its tenant, time, dimension values and measures.
func (e *Engine) Resolve(src *config.Source, row Row) (*Fact, error) {
	tenant, err := parseTenant(row[e.cfg.Tenancy.TenantColumn])
	if err != nil {
		return nil, err
	}
	ts, err := ParseTimestamp(row[src.Time], e.loc)
	if err != nil {
		return nil, fmt.Errorf("column %s: %w", src.Time, err)
	}
	f := &Fact{Tenant: tenant, Time: ts, Dims: map[string]string{}, Measures: make([]int64, src.Measures.Len())}
	for i, m := range src.Measures.Values() {
		v, err := MeasureValue(m, row)
		if err != nil {
			return nil, err
		}
		f.Measures[i] = v
	}
	for _, dim := range src.Dimensions.Values() {
		f.Dims[dim.Name] = e.dimValue(dim, tenant, row)
	}
	return f, nil
}

// MeasureValue returns a row's contribution to one measure, as an exact integer.
func MeasureValue(m *config.Measure, row Row) (int64, error) {
	if m.Type == "count" {
		return 1, nil
	}
	v, ok := row[m.Column]
	if !ok {
		return 0, nil // SUM ignores NULLs
	}
	if m.MinorUnits || (m.Scale == 0 && !strings.Contains(v, ".")) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("measure %s: column %s value %q is not an integer", m.Name, m.Column, v)
		}
		return n, nil
	}
	return decimal.Parse(v, m.Scale)
}

func (e *Engine) dimValue(dim *config.Dimension, tenant int32, row Row) string {
	if dim.Join == nil {
		if v, ok := row[dim.Column]; ok {
			return v
		}
		return Null
	}
	on, ok := row[dim.Join.On]
	if !ok {
		return Null
	}
	r := e.dims[DimKey{Table: dim.Join.Table, Tenant: tenant, Key: on}]
	if r == nil {
		return Null
	}
	if v, ok := r[dim.Join.Column]; ok {
		return v
	}
	return Null
}

// prefetch loads every dimension row the transaction's facts join to and the
// engine has not seen, in one batch.
func (e *Engine) prefetch(txn *Txn) error {
	if e.load == nil {
		return nil
	}
	want := map[DimKey]bool{}
	for _, c := range txn.Changes {
		src := e.cfg.Sources.Get(c.Table)
		if src == nil || src.Time == "" {
			continue
		}
		for _, row := range []Row{c.Old, c.New} {
			if row == nil {
				continue
			}
			t, err := parseTenant(row[e.cfg.Tenancy.TenantColumn])
			if err != nil {
				continue
			}
			for _, dim := range src.Dimensions.Values() {
				if dim.Join == nil {
					continue
				}
				if on, ok := row[dim.Join.On]; ok {
					k := DimKey{Table: dim.Join.Table, Tenant: t, Key: on}
					if _, seen := e.dims[k]; !seen {
						want[k] = true
					}
				}
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	keys := make([]DimKey, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	got, err := e.load(keys)
	if err != nil {
		return fmt.Errorf("load dimension rows: %w", err)
	}
	for _, k := range keys {
		if r, ok := got[k]; ok {
			e.dims[k] = r
		}
	}
	return nil
}

// SeedDimension records a dimension row, as the mirror holds it. Used after a
// backfill or rebuild and by tests.
func (e *Engine) SeedDimension(k DimKey, r Row) { e.dims[k] = r }

// ForgetDimensions drops the dimension cache (after a rebuild from main).
func (e *Engine) ForgetDimensions() { e.dims = map[DimKey]Row{} }

// ForgetTenant drops one store's cached dimension rows, after that store alone
// was reloaded from its database.
func (e *Engine) ForgetTenant(tenant int32) {
	for k := range e.dims {
		if k.Tenant == tenant {
			delete(e.dims, k)
		}
	}
}

func (e *Engine) contribute(src *config.Source, f *Fact, sign int64, d *Delta) {
	e.contributeTo(e.cfg.RollupsFor(src.Name), f, sign, d)
}

func (e *Engine) contributeTo(rollups []*config.Rollup, f *Fact, sign int64, d *Delta) {
	now := e.now()
	for _, r := range rollups {
		var k [config.MaxDims]string
		for i, dim := range r.Dims {
			k[i] = f.Dims[dim]
		}
		for _, g := range timeutil.Grains {
			if !r.HasGrain(g) {
				continue
			}
			b := timeutil.Trunc(f.Time, g, e.loc)
			if b.Before(r.Since(g, now, e.loc)) {
				continue
			}
			d.add(CellKey{Tenant: f.Tenant, Rollup: r.ID, Grain: g, Bucket: b.Unix(), K: k}, f.Measures, sign)
		}
	}
	d.touch(f.Tenant, f.Time)
}

// CellsFrom recomputes the cells of the given rollups (all over the facts'
// source) from mirror rows. It is
// the in-memory form of a rebuild from the mirror and is what the SQL rebuild
// must equal.
func (e *Engine) CellsFrom(rollups []*config.Rollup, facts []*Fact) map[CellKey][]int64 {
	d := NewDelta()
	for _, f := range facts {
		e.contributeTo(rollups, f, 1, d)
	}
	return d.NonZeroCells()
}

// AffectedRollups returns the rollups that read a join dimension through the
// given dimension table, which must be rebuilt when its join columns change.
func AffectedRollups(cfg *config.Config, table string) []*config.Rollup {
	var out []*config.Rollup
	for _, r := range cfg.Rollups.Values() {
		src := cfg.Sources.Get(r.Source)
		for _, dn := range r.Dims {
			if d := src.Dimensions.Get(dn); d != nil && d.Join != nil && d.Join.Table == table {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

func parseTenant(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("tenant id %q is not an integer", s)
	}
	return int32(n), nil
}

var tsLayouts = []string{
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-07:00:00",
	time.RFC3339Nano,
}

// ParseTimestamp reads a timestamp in Postgres text output. Values without an
// offset (timestamp without time zone) are wall-clock times in loc.
func ParseTimestamp(s string, loc *time.Location) (time.Time, error) {
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot read timestamp %q", s)
}
