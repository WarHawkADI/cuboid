// Package backfill loads the aggregate store from a consistent snapshot of
// the main database: every source table is streamed with COPY into the
// mirror, then every rollup is built with set-based SQL on AGG. The main
// database only serves sequential reads, and each chunk waits for the store
// app's canary to be healthy first.
package backfill

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/store"
)

// Options configure one backfill.
type Options struct {
	MainDSN  string // cuboid_cdc
	Snapshot string // exported by the slot creation
	Store    *store.Store
	Cfg      *config.Config
	Specs    map[string]*store.Table
	Names    store.Names
	Loc      *time.Location
	Now      func() time.Time
	Workers  int
	// Throttle blocks while the main database is under pressure (canary).
	Throttle func(ctx context.Context)
	Log      *slog.Logger
	Progress func(phase string, done, total int64)
}

// Stats summarise a backfill.
type Stats struct {
	Rows      int64
	CopyTime  time.Duration
	BuildTime time.Duration
}

type chunk struct {
	table  string
	tenant int32
}

// Run performs the backfill into the tables named by o.Names.
func Run(ctx context.Context, o Options) (Stats, error) {
	var st Stats
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if err := truncate(ctx, o); err != nil {
		return st, err
	}
	start := time.Now()
	chunks, err := listChunks(ctx, o)
	if err != nil {
		return st, err
	}
	o.Log.Info("backfill: copying from snapshot", "snapshot", o.Snapshot, "chunks", len(chunks))
	work := make(chan chunk)
	var rows, done atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	for w := 0; w < o.Workers; w++ {
		g.Go(func() error { return worker(gctx, o, work, &rows, &done, int64(len(chunks))) })
	}
	g.Go(func() error {
		defer close(work)
		for _, c := range chunks {
			select {
			case work <- c:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return st, err
	}
	st.Rows = rows.Load()
	st.CopyTime = time.Since(start)
	o.Log.Info("backfill: mirror loaded", "rows", st.Rows, "took", st.CopyTime.Round(time.Millisecond))

	start = time.Now()
	if err := BuildAll(ctx, o.Store, o.Cfg, o.Loc, o.Now(), o.Names, nil, o.Cfg.Rollups.Values(), o.Progress); err != nil {
		return st, err
	}
	st.BuildTime = time.Since(start)
	o.Log.Info("backfill: rollups built", "took", st.BuildTime.Round(time.Millisecond))
	return st, nil
}

func truncate(ctx context.Context, o Options) error {
	var tables []string
	for _, name := range o.Cfg.Sources.Keys() {
		tables = append(tables, o.Names.Mirror(name))
	}
	for _, src := range o.Cfg.FactSources() {
		tables = append(tables, o.Names.Cells(src.Name))
	}
	_, err := o.Store.W.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", "))
	return err
}

func snapshotTx(ctx context.Context, conn *pgx.Conn, snapshot string) (pgx.Tx, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", strings.ReplaceAll(snapshot, "'", ""))); err != nil {
		tx.Rollback(ctx)
		return nil, fmt.Errorf("import snapshot %s: %w", snapshot, err)
	}
	return tx, nil
}

// listChunks splits every table by tenant, biggest tables first.
func listChunks(ctx context.Context, o Options) ([]chunk, error) {
	conn, err := cdc.ConnectPlain(ctx, o.MainDSN, "cuboid-backfill")
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	tx, err := snapshotTx(ctx, conn, o.Snapshot)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tcol := store.QuoteIdent(o.Cfg.Tenancy.TenantColumn)
	var out []chunk
	order := append(fact(o.Cfg), dims(o.Cfg)...)
	for _, name := range order {
		rows, err := tx.Query(ctx, fmt.Sprintf("SELECT DISTINCT %s FROM %s ORDER BY 1", tcol, store.QuoteIdent(name)))
		if err != nil {
			return nil, fmt.Errorf("list tenants of %s: %w", name, err)
		}
		for rows.Next() {
			var t int32
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, chunk{table: name, tenant: t})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fact(cfg *config.Config) []string {
	var out []string
	for _, s := range cfg.FactSources() {
		out = append(out, s.Name)
	}
	return out
}

func dims(cfg *config.Config) []string {
	var out []string
	for _, s := range cfg.Sources.Values() {
		if s.Time == "" {
			out = append(out, s.Name)
		}
	}
	return out
}

func worker(ctx context.Context, o Options, work <-chan chunk, rows, done *atomic.Int64, total int64) error {
	main, err := cdc.ConnectPlain(ctx, o.MainDSN, "cuboid-backfill")
	if err != nil {
		return err
	}
	defer main.Close(context.Background())
	tx, err := snapshotTx(ctx, main, o.Snapshot)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	agg, err := o.Store.W.Acquire(ctx)
	if err != nil {
		return err
	}
	defer agg.Release()
	tcol := store.QuoteIdent(o.Cfg.Tenancy.TenantColumn)
	for c := range work {
		if o.Throttle != nil {
			o.Throttle(ctx)
		}
		spec := o.Specs[c.table]
		var cols []string
		for _, n := range spec.ColumnNames() {
			cols = append(cols, store.QuoteIdent(n))
		}
		list := strings.Join(cols, ", ")
		pr, pw := io.Pipe()
		copyErr := make(chan error, 1)
		go func() {
			_, err := tx.Conn().PgConn().CopyTo(ctx, pw, fmt.Sprintf("COPY (SELECT %s FROM %s WHERE %s = %d) TO STDOUT",
				list, store.QuoteIdent(c.table), tcol, c.tenant))
			pw.CloseWithError(err)
			copyErr <- err
		}()
		tag, err := agg.Conn().PgConn().CopyFrom(ctx, pr, fmt.Sprintf("COPY %s (%s) FROM STDIN", o.Names.Mirror(c.table), list))
		pr.CloseWithError(err)
		if cerr := <-copyErr; cerr != nil {
			return fmt.Errorf("copy %s for store %d from the main database: %w", c.table, c.tenant, cerr)
		}
		if err != nil {
			return fmt.Errorf("copy %s for store %d into the mirror: %w", c.table, c.tenant, err)
		}
		rows.Add(tag.RowsAffected())
		n := done.Add(1)
		if o.Progress != nil {
			o.Progress("copy", n, total)
		}
	}
	return tx.Commit(ctx)
}

// BuildAll seeds the dictionary and builds the given rollups from the mirror
// for tenants (nil = all), in parallel, then refreshes planner statistics.
func BuildAll(ctx context.Context, st *store.Store, cfg *config.Config, loc *time.Location, now time.Time, n store.Names, tenants []int32, rollups []*config.Rollup, progress func(string, int64, int64)) error {
	for _, sql := range store.SeedDictionary(cfg, n) {
		if _, err := st.W.Exec(ctx, sql); err != nil {
			return fmt.Errorf("seed dictionary: %w", err)
		}
	}
	var done atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, r := range rollups {
		g.Go(func() error {
			for _, s := range store.BuildRollup(cfg, r, loc, now, n, tenants) {
				if _, err := st.W.Exec(gctx, s.SQL, s.Args...); err != nil {
					return fmt.Errorf("build rollup %s: %w", r.Name, err)
				}
			}
			if progress != nil {
				progress("build", done.Add(1), int64(len(rollups)))
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	var tables []string
	for _, name := range cfg.Sources.Keys() {
		tables = append(tables, n.Mirror(name))
	}
	for _, src := range cfg.FactSources() {
		tables = append(tables, n.Cells(src.Name))
	}
	_, err := st.W.Exec(ctx, "ANALYZE "+strings.Join(tables, ", "))
	return err
}
