// Package backfill loads the aggregate store from consistent snapshots of the
// source databases, one source at a time (ADR-012): the platform's database,
// or one store's. A load replaces everything the aggregate store holds from
// that source in a single transaction, so readers see the old numbers or the
// new ones, never a mix, and every other store keeps streaming meanwhile.
// Stores load in parallel, each in its own transaction.
package backfill

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"

	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/store"
)

// Source is one source database to load.
type Source struct {
	Name   string // the database
	DSN    string // cuboid_cdc, on that database
	Tenant int32  // the store whose database it is; 0 when rows carry their own
	Tables []*config.Source
	Slot   string // the replication slot to create; its stream resumes the source
}

// Options configure one load.
type Options struct {
	Store *store.Store
	Cfg   *config.Config
	Specs map[string]*store.Table
	Loc   *time.Location
	Now   func() time.Time
	// Throttle blocks while the main server is under pressure (canary).
	Throttle func(ctx context.Context)
	Log      *slog.Logger
}

// Stats summarise a load.
type Stats struct {
	Rows      int64
	CopyTime  time.Duration
	BuildTime time.Duration
}

// Load creates the source's slot with an exported snapshot, copies the
// source's tables from that snapshot, builds their rollups and records the
// slot and its consistent point, all in one aggregate-store transaction. The
// stream that resumes from the consistent point misses nothing and repeats
// nothing. On failure the new slot is dropped and nothing changes.
func Load(ctx context.Context, src Source, o Options) (pglogrepl.LSN, Stats, error) {
	var st Stats
	repl, err := cdc.ConnectReplication(ctx, src.DSN)
	if err != nil {
		return 0, st, fmt.Errorf("connect to %s for replication: %w", src.Name, err)
	}
	defer repl.Close(context.Background())
	stream := &cdc.Stream{Conn: repl, Slot: src.Slot}
	consistent, snapshot, err := stream.CreateSlot(ctx, false)
	if err != nil {
		return 0, st, err
	}
	ok := false
	defer func() {
		if !ok {
			dropSlot(src, o.Log)
		}
	}()
	// The exported snapshot lives while repl stays idle: every read below
	// imports it on a connection of its own.
	conn, err := cdc.ConnectPlain(ctx, src.DSN, "cuboid-backfill")
	if err != nil {
		return 0, st, err
	}
	defer conn.Close(context.Background())
	snap, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, st, err
	}
	defer snap.Rollback(context.Background())
	if _, err := snap.Exec(ctx, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", strings.ReplaceAll(snapshot, "'", ""))); err != nil {
		return 0, st, fmt.Errorf("import snapshot %s of %s: %w", snapshot, src.Name, err)
	}
	if err := seedDictionary(ctx, snap, src, o); err != nil {
		return 0, st, err
	}

	tx, err := o.Store.W.Begin(ctx)
	if err != nil {
		return 0, st, err
	}
	defer tx.Rollback(context.Background())
	start := time.Now()
	tcol := o.Cfg.Tenancy.TenantColumn
	for _, t := range src.Tables {
		del, args := fmt.Sprintf("DELETE FROM %s", store.Names{}.Mirror(t.Name)), []any{}
		if src.Tenant != 0 {
			del, args = del+" WHERE "+store.QuoteIdent(tcol)+" = $1", []any{src.Tenant}
		}
		if _, err := tx.Exec(ctx, del, args...); err != nil {
			return 0, st, fmt.Errorf("clear %s of %s: %w", t.Name, src.Name, err)
		}
		if t.Time != "" {
			del, args := fmt.Sprintf("DELETE FROM %s", store.Names{}.Cells(t.Name)), []any{}
			if src.Tenant != 0 {
				del, args = del+" WHERE tenant_id = $1", []any{src.Tenant}
			}
			if _, err := tx.Exec(ctx, del, args...); err != nil {
				return 0, st, fmt.Errorf("clear the rollups of %s for %s: %w", t.Name, src.Name, err)
			}
		}
		if o.Throttle != nil {
			o.Throttle(ctx)
		}
		n, err := copyTable(ctx, snap, tx, src, t, o)
		if err != nil {
			return 0, st, err
		}
		st.Rows += n
	}
	st.CopyTime = time.Since(start)

	start = time.Now()
	var tenants []int32
	if src.Tenant != 0 {
		tenants = []int32{src.Tenant}
	}
	for _, t := range src.Tables {
		for _, r := range o.Cfg.RollupsFor(t.Name) {
			for _, s := range store.BuildRollup(o.Cfg, r, o.Loc, o.Now(), store.Names{}, tenants) {
				if _, err := tx.Exec(ctx, s.SQL, s.Args...); err != nil {
					return 0, st, fmt.Errorf("build rollup %s for %s: %w", r.Name, src.Name, err)
				}
			}
		}
	}
	st.BuildTime = time.Since(start)

	var tenant any
	if src.Tenant != 0 {
		tenant = src.Tenant
		if _, err := tx.Exec(ctx, `INSERT INTO data_versions (tenant_id, version, changed_at) VALUES ($1, 1, now())
			ON CONFLICT (tenant_id) DO UPDATE SET version = data_versions.version + 1, changed_at = now()`, src.Tenant); err != nil {
			return 0, st, err
		}
	}
	// The snapshot is complete as of now: that is the data's age until the
	// stream's first heartbeat arrives.
	if _, err := tx.Exec(ctx, `INSERT INTO cdc_sources (source, tenant_id, slot, applied_lsn, applied_commit_time, heartbeat_time, backfilled_at)
		VALUES ($1, $2, $3, $4::pg_lsn, now(), now(), now())
		ON CONFLICT (source) DO UPDATE SET tenant_id = excluded.tenant_id, slot = excluded.slot, applied_lsn = excluded.applied_lsn,
			applied_commit_time = excluded.applied_commit_time, heartbeat_time = excluded.heartbeat_time, backfilled_at = excluded.backfilled_at`,
		src.Name, tenant, src.Slot, consistent.String()); err != nil {
		return 0, st, fmt.Errorf("record the position of %s: %w", src.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, st, fmt.Errorf("commit the load of %s: %w", src.Name, err)
	}
	ok = true
	snap.Commit(ctx)
	return consistent, st, nil
}

// copyTable streams one table from the source's snapshot into the mirror, in
// the load's transaction. The mirror forces row-level security, under which
// Postgres refuses COPY FROM, so rows land in a temporary table first. A
// store's rows are attributed to that store: its database decides whose
// they are, exactly as the stream does.
func copyTable(ctx context.Context, snap pgx.Tx, tx pgx.Tx, src Source, t *config.Source, o Options) (int64, error) {
	spec := o.Specs[t.Name]
	if spec == nil {
		return 0, fmt.Errorf("table %s: not found in the catalog", t.Name)
	}
	tcol := o.Cfg.Tenancy.TenantColumn
	var cols, sel []string
	for _, n := range spec.ColumnNames() {
		cols = append(cols, store.QuoteIdent(n))
		if n == tcol && src.Tenant != 0 {
			sel = append(sel, fmt.Sprintf("%d", src.Tenant))
			continue
		}
		sel = append(sel, store.QuoteIdent(n))
	}
	list := strings.Join(cols, ", ")
	tmp := store.QuoteIdent("load_" + t.Name)
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE %s (LIKE %s) ON COMMIT DROP", tmp, store.Names{}.Mirror(t.Name))); err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	copyErr := make(chan error, 1)
	go func() {
		_, err := snap.Conn().PgConn().CopyTo(ctx, pw, fmt.Sprintf("COPY (SELECT %s FROM %s) TO STDOUT", strings.Join(sel, ", "), store.QuoteIdent(t.Name)))
		pw.CloseWithError(err)
		copyErr <- err
	}()
	tag, err := tx.Conn().PgConn().CopyFrom(ctx, pr, fmt.Sprintf("COPY %s (%s) FROM STDIN", tmp, list))
	pr.CloseWithError(err)
	if cerr := <-copyErr; cerr != nil {
		return 0, fmt.Errorf("copy %s from %s: %w", t.Name, src.Name, cerr)
	}
	if err != nil {
		return 0, fmt.Errorf("copy %s of %s into the mirror: %w", t.Name, src.Name, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", store.Names{}.Mirror(t.Name), list, list, tmp)); err != nil {
		return 0, fmt.Errorf("load %s of %s into the mirror: %w", t.Name, src.Name, err)
	}
	return tag.RowsAffected(), nil
}

// seedDictionary inserts the source's text dimension values into the shared
// dictionary in a short transaction of its own, in a fixed order, so stores
// loading at the same time never block or deadlock on the same values.
func seedDictionary(ctx context.Context, snap pgx.Tx, src Source, o Options) error {
	var dims, vals []string
	for _, ds := range store.DictionarySources(o.Cfg, src.Tables) {
		rows, err := snap.Query(ctx, ds.SQL)
		if err != nil {
			return fmt.Errorf("list %s values in %s: %w", ds.Dim, src.Name, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			dims, vals = append(dims, ds.Dim), append(vals, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if len(dims) == 0 {
		return nil
	}
	idx := make([]int, len(dims))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		if dims[idx[a]] != dims[idx[b]] {
			return dims[idx[a]] < dims[idx[b]]
		}
		return vals[idx[a]] < vals[idx[b]]
	})
	sd, sv := make([]string, len(idx)), make([]string, len(idx))
	for i, j := range idx {
		sd[i], sv[i] = dims[j], vals[j]
	}
	_, err := o.Store.W.Exec(ctx, `INSERT INTO dict (dim, value) SELECT * FROM unnest($1::text[], $2::text[])
		ON CONFLICT (dim, value) DO NOTHING`, sd, sv)
	if err != nil {
		return fmt.Errorf("seed the dictionary from %s: %w", src.Name, err)
	}
	return nil
}

func dropSlot(src Source, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e := cdc.NewEmitter(src.DSN)
	defer e.Close()
	if err := e.DropSlot(ctx, src.Slot); err != nil && log != nil {
		log.Warn("drop the slot of a failed load", "slot", src.Slot, "err", err)
	}
}

// Analyze refreshes the planner's statistics of every mirror and cell table,
// after a batch of loads.
func Analyze(ctx context.Context, st *store.Store, cfg *config.Config) error {
	var tables []string
	for _, name := range cfg.Sources.Keys() {
		tables = append(tables, store.Names{}.Mirror(name))
	}
	for _, src := range cfg.FactSources() {
		tables = append(tables, store.Names{}.Cells(src.Name))
	}
	_, err := st.W.Exec(ctx, "ANALYZE "+strings.Join(tables, ", "))
	return err
}
