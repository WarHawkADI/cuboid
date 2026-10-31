// Package flush writes a coalesced engine.Delta to the aggregate store in one
// transaction: mirror rows, rollup cell deltas, the applied LSN, tenant data
// versions and security events. Because the position commits with the data,
// a crash at any point either keeps the whole flush or none of it, and the
// stream resumes from the committed position (exactly once).
package flush

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/store"
)

// Position is how far the stream has been applied.
type Position struct {
	LSN        string // pg_lsn text, e.g. 0/3A1F2C8
	CommitTime time.Time
	Heartbeat  time.Time
}

// Result describes a committed flush.
type Result struct {
	Versions   map[int32]int64
	Touched    map[int32]*engine.Touch
	Cells      int
	MirrorRows int
	Rebuilt    []string // "store 42: status_category" for scoped dimension rebuilds
	Duration   time.Duration
	CellDeltas map[engine.CellKey][]int64 // the cell deltas written, for an online rebuild's delta log
}

// Flusher writes deltas.
type Flusher struct {
	Store  *store.Store
	Specs  map[string]*store.Table
	Dict   *store.Dict
	Loc    *time.Location
	Now    func() time.Time
	Names  store.Names
	Log    *slog.Logger
	Notify string // NOTIFY channel; empty disables
}

// Flush writes d at position pos. cfg is the configuration d was built with.
func (f *Flusher) Flush(ctx context.Context, cfg *config.Config, d *engine.Delta, pos Position) (*Result, error) {
	start := time.Now()
	tx, err := f.Store.W.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())

	res := &Result{Touched: d.Touched, Versions: map[int32]int64{}}
	cells := d.NonZeroCells()

	// Rollups rebuilt from the mirror in this flush supersede their deltas.
	rebuild := map[int32]map[int16]*config.Rollup{}
	for tenant, tables := range d.DimRebuild {
		for table := range tables {
			for _, r := range engine.AffectedRollups(cfg, table) {
				if rebuild[tenant] == nil {
					rebuild[tenant] = map[int16]*config.Rollup{}
				}
				rebuild[tenant][r.ID] = r
			}
		}
	}
	for k := range cells {
		if rebuild[k.Tenant][k.Rollup] != nil {
			delete(cells, k)
		}
	}

	enc := store.Encoder{Cfg: cfg, Dict: f.Dict}
	want := f.dictValues(cfg, d, cells)
	ids, err := f.Dict.Resolve(ctx, tx, want)
	if err != nil {
		return nil, err
	}
	n, err := f.writeMirror(ctx, tx, cfg, d)
	if err != nil {
		return nil, err
	}
	res.MirrorRows = n
	if err := f.writeCells(ctx, tx, cfg, enc, ids, cells); err != nil {
		return nil, err
	}
	res.Cells = len(cells)
	res.CellDeltas = cells
	for tenant, rs := range rebuild {
		for _, r := range rs {
			if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = $1 AND rollup = $2", f.Names.Cells(r.Source)), tenant, r.ID); err != nil {
				return nil, err
			}
			for _, st := range store.BuildRollup(cfg, r, f.Loc, f.Now(), f.Names, []int32{tenant}) {
				if _, err := tx.Exec(ctx, st.SQL, st.Args...); err != nil {
					return nil, fmt.Errorf("rebuild %s for store %d from the mirror: %w", r.Name, tenant, err)
				}
			}
			res.Rebuilt = append(res.Rebuilt, fmt.Sprintf("store %d: %s", tenant, r.Name))
		}
	}

	if pos.LSN != "" {
		if _, err := tx.Exec(ctx, `UPDATE cdc_state SET applied_lsn = $1::pg_lsn,
			applied_commit_time = coalesce($2, applied_commit_time), heartbeat_time = coalesce($3, heartbeat_time),
			flushes = flushes + 1`, pos.LSN, nullTime(pos.CommitTime), nullTime(pos.Heartbeat)); err != nil {
			return nil, fmt.Errorf("record applied position: %w", err)
		}
	}
	if len(d.Touched) > 0 {
		tenants := make([]int32, 0, len(d.Touched))
		for t := range d.Touched {
			tenants = append(tenants, t)
		}
		rows, err := tx.Query(ctx, `INSERT INTO data_versions (tenant_id, version, changed_at)
			SELECT t, 1, now() FROM unnest($1::int[]) AS t
			ON CONFLICT (tenant_id) DO UPDATE SET version = data_versions.version + 1, changed_at = now()
			RETURNING tenant_id, version`, tenants)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t int32
			var v int64
			if err := rows.Scan(&t, &v); err != nil {
				rows.Close()
				return nil, err
			}
			res.Versions[t] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, ev := range d.Events {
		if _, err := tx.Exec(ctx, `INSERT INTO security_events (kind, severity, tenant_id, detail) VALUES ($1, $2, $3, $4)`,
			ev.Kind, ev.Severity, ev.Tenant, ev.Detail); err != nil {
			return nil, err
		}
	}
	if f.Notify != "" && len(d.Touched) > 0 {
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", f.Notify, pos.LSN); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit flush: %w", err)
	}
	f.Dict.Remember(ids)
	res.Duration = time.Since(start)
	return res, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// dictValues lists every text dimension value the flush writes, so the
// dictionary holds them before cells, mirror rows or edge reads need them.
func (f *Flusher) dictValues(cfg *config.Config, d *engine.Delta, cells map[engine.CellKey][]int64) map[store.DimValue]bool {
	want := map[store.DimValue]bool{}
	for k := range cells {
		r := cfg.RollupByID(k.Rollup)
		src := cfg.Sources.Get(r.Source)
		for i, dim := range r.Dims {
			if dd := src.Dimensions.Get(dim); !dd.IsKey() && k.K[i] != engine.Null {
				want[store.DimValue{Dim: dim, Value: k.K[i]}] = true
			}
		}
	}
	for table, ops := range d.Mirror {
		for _, op := range ops {
			if op.Row == nil {
				continue
			}
			for _, src := range cfg.FactSources() {
				for _, dim := range src.Dimensions.Values() {
					if dim.IsKey() {
						continue
					}
					col := ""
					switch {
					case dim.Join == nil && src.Name == table:
						col = dim.Column
					case dim.Join != nil && dim.Join.Table == table:
						col = dim.Join.Column
					}
					if v, ok := op.Row[col]; ok && col != "" {
						want[store.DimValue{Dim: dim.Name, Value: v}] = true
					}
				}
			}
		}
	}
	return want
}

func (f *Flusher) writeMirror(ctx context.Context, tx pgx.Tx, cfg *config.Config, d *engine.Delta) (int, error) {
	total := 0
	for table, ops := range d.Mirror {
		spec := f.Specs[table]
		if spec == nil {
			continue
		}
		cols := spec.ColumnNames()
		delKeys := make([][]*string, len(spec.Key))
		upserts := make([][]*string, len(cols))
		nUp := 0
		for _, op := range ops {
			if op.Row == nil {
				for i, k := range spec.Key {
					delKeys[i] = append(delKeys[i], val(op.Old, k))
				}
				continue
			}
			for i, c := range cols {
				upserts[i] = append(upserts[i], val(op.Row, c))
			}
			nUp++
		}
		name := f.Names.Mirror(table)
		if len(delKeys) > 0 && len(delKeys[0]) > 0 {
			var unn, alias, conds []string
			args := make([]any, len(spec.Key))
			for i, k := range spec.Key {
				unn = append(unn, fmt.Sprintf("$%d::text[]", i+1))
				alias = append(alias, "u"+strconv.Itoa(i))
				conds = append(conds, fmt.Sprintf("m.%s = u.u%d::%s", store.QuoteIdent(k), i, spec.Type(k)))
				args[i] = delKeys[i]
			}
			sql := fmt.Sprintf("DELETE FROM %s m USING unnest(%s) AS u(%s) WHERE %s",
				name, strings.Join(unn, ", "), strings.Join(alias, ", "), strings.Join(conds, " AND "))
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				return 0, fmt.Errorf("delete mirror rows in %s: %w", table, err)
			}
			total += len(delKeys[0])
		}
		if nUp > 0 {
			var unn, alias, casts, sets []string
			args := make([]any, len(cols))
			for i, c := range cols {
				unn = append(unn, fmt.Sprintf("$%d::text[]", i+1))
				alias = append(alias, "u"+strconv.Itoa(i))
				casts = append(casts, fmt.Sprintf("u.u%d::%s", i, spec.Columns[i].Type))
				sets = append(sets, fmt.Sprintf("%s = EXCLUDED.%s", store.QuoteIdent(c), store.QuoteIdent(c)))
				args[i] = upserts[i]
			}
			var quoted []string
			for _, c := range cols {
				quoted = append(quoted, store.QuoteIdent(c))
			}
			var keys []string
			for _, k := range spec.Key {
				keys = append(keys, store.QuoteIdent(k))
			}
			sql := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM unnest(%s) AS u(%s) ON CONFLICT (%s) DO UPDATE SET %s",
				name, strings.Join(quoted, ", "), strings.Join(casts, ", "), strings.Join(unn, ", "), strings.Join(alias, ", "),
				strings.Join(keys, ", "), strings.Join(sets, ", "))
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				return 0, fmt.Errorf("upsert mirror rows in %s: %w", table, err)
			}
			total += nUp
		}
	}
	return total, nil
}

func val(r engine.Row, col string) *string {
	if v, ok := r[col]; ok {
		return &v
	}
	return nil
}

func (f *Flusher) writeCells(ctx context.Context, tx pgx.Tx, cfg *config.Config, enc store.Encoder, ids map[store.DimValue]int64, cells map[engine.CellKey][]int64) error {
	bySource := map[string][]engine.CellKey{}
	for k := range cells {
		r := cfg.RollupByID(k.Rollup)
		bySource[r.Source] = append(bySource[r.Source], k)
	}
	for source, keys := range bySource {
		src := cfg.Sources.Get(source)
		nm := src.Measures.Len()
		var tenant []int32
		var rollup, grain []int16
		var bucket []time.Time
		k := [3][]int64{}
		m := make([][]int64, nm)
		for _, ck := range keys {
			r := cfg.RollupByID(ck.Rollup)
			var kid [3]int64
			for i, dim := range r.Dims {
				v := ck.K[i]
				if v == engine.Null {
					continue
				}
				if src.Dimensions.Get(dim).IsKey() {
					id, err := strconv.ParseInt(v, 10, 64)
					if err != nil {
						return fmt.Errorf("rollup %s: %s value %q is not an integer", r.Name, dim, v)
					}
					kid[i] = id
				} else {
					id, ok := ids[store.DimValue{Dim: dim, Value: v}]
					if !ok {
						return fmt.Errorf("rollup %s: no dictionary id for %s=%q", r.Name, dim, v)
					}
					kid[i] = id
				}
			}
			tenant = append(tenant, ck.Tenant)
			rollup = append(rollup, ck.Rollup)
			grain = append(grain, int16(ck.Grain))
			bucket = append(bucket, time.Unix(ck.Bucket, 0))
			for i := range kid {
				k[i] = append(k[i], kid[i])
			}
			for i, x := range cells[ck] {
				m[i] = append(m[i], x)
			}
		}
		var mcols, munn, sets, zero []string
		args := []any{tenant, rollup, grain, bucket, k[0], k[1], k[2]}
		for i, meas := range src.Measures.Values() {
			c := store.QuoteIdent(meas.Name)
			mcols = append(mcols, c)
			munn = append(munn, fmt.Sprintf("$%d::bigint[]", 8+i))
			sets = append(sets, fmt.Sprintf("%s = c.%s + EXCLUDED.%s", c, c, c))
			zero = append(zero, fmt.Sprintf("c.%s = 0", c))
			args = append(args, m[i])
		}
		table := f.Names.Cells(source)
		sql := fmt.Sprintf(`INSERT INTO %s AS c (tenant_id, rollup, grain, bucket, k1, k2, k3, %s)
			SELECT * FROM unnest($1::int[], $2::smallint[], $3::smallint[], $4::timestamptz[], $5::bigint[], $6::bigint[], $7::bigint[], %s)
			ON CONFLICT (tenant_id, rollup, grain, bucket, k1, k2, k3) DO UPDATE SET %s
			RETURNING tenant_id, rollup, grain, bucket, k1, k2, k3, (%s)`,
			table, strings.Join(mcols, ", "), strings.Join(munn, ", "), strings.Join(sets, ", "), strings.Join(zero, " AND "))
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("upsert %s cells: %w", source, err)
		}
		var zt []int32
		var zr, zg []int16
		var zb []time.Time
		var z1, z2, z3 []int64
		for rows.Next() {
			var t int32
			var r, g int16
			var b time.Time
			var a, bb, c int64
			var isZero bool
			if err := rows.Scan(&t, &r, &g, &b, &a, &bb, &c, &isZero); err != nil {
				rows.Close()
				return err
			}
			if isZero {
				zt, zr, zg, zb = append(zt, t), append(zr, r), append(zg, g), append(zb, b)
				z1, z2, z3 = append(z1, a), append(z2, bb), append(z3, c)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("upsert %s cells: %w", source, err)
		}
		// A cell whose every measure returned to zero is an empty group: delete
		// it so cells only exist for groups that have rows.
		if len(zt) > 0 {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s c USING unnest($1::int[], $2::smallint[], $3::smallint[], $4::timestamptz[], $5::bigint[], $6::bigint[], $7::bigint[]) AS u(t, r, g, b, k1, k2, k3)
				WHERE c.tenant_id = u.t AND c.rollup = u.r AND c.grain = u.g AND c.bucket = u.b AND c.k1 = u.k1 AND c.k2 = u.k2 AND c.k3 = u.k3`, table),
				zt, zr, zg, zb, z1, z2, z3); err != nil {
				return err
			}
		}
	}
	return nil
}
