// Package store owns the aggregate store (AGG): its schema, the dictionary,
// the SQL that builds rollups from the mirror, and tenant-scoped reads under
// row-level security.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Store holds the two AGG pools: the writer (flusher, rebuilds) and the
// reader (queries, always under RLS).
type Store struct {
	W *pgxpool.Pool
	R *pgxpool.Pool
}

// Open connects both pools and checks each connects as the expected role.
func Open(ctx context.Context, writerDSN, readerDSN string, readerConns int32) (*Store, error) {
	w, err := connect(ctx, writerDSN, 8, "cuboid_writer")
	if err != nil {
		return nil, err
	}
	r, err := connect(ctx, readerDSN, readerConns, "cuboid_reader")
	if err != nil {
		w.Close()
		return nil, err
	}
	return &Store{W: w, R: r}, nil
}

func connect(ctx context.Context, dsn string, max int32, wantRole string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse %s DSN: %w", wantRole, err)
	}
	cfg.MaxConns = max
	cfg.ConnConfig.RuntimeParams["application_name"] = "cuboid"
	var pool *pgxpool.Pool
	for attempt := 0; ; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			err = pool.Ping(ctx)
		}
		if err == nil {
			break
		}
		if pool != nil {
			pool.Close()
		}
		if attempt >= 30 || ctx.Err() != nil {
			return nil, fmt.Errorf("connect to aggregate store as %s: %w", wantRole, err)
		}
		time.Sleep(time.Second)
	}
	var role string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		pool.Close()
		return nil, err
	}
	if role != wantRole {
		pool.Close()
		return nil, fmt.Errorf("aggregate store DSN connects as %q, expected %q", role, wantRole)
	}
	return pool, nil
}

// Close closes both pools.
func (s *Store) Close() {
	s.W.Close()
	s.R.Close()
}

// ApplySchema creates the static tables.
func (s *Store) ApplySchema(ctx context.Context) error {
	if _, err := s.W.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply aggregate store schema: %w", err)
	}
	return nil
}

// Freshness is the state of the aggregate store as seen by one read.
type Freshness struct {
	LSN        string
	CommitTime time.Time // commit time of the latest applied source transaction
	Heartbeat  time.Time // send time of the latest applied heartbeat
}

// AsOf is the instant the data is known to be complete up to.
func (f Freshness) AsOf() time.Time {
	if f.Heartbeat.After(f.CommitTime) {
		return f.Heartbeat
	}
	return f.CommitTime
}

// ReadFreshness returns how fresh the given stores' data is: the oldest of
// their source databases, since an answer over several stores is only as
// fresh as its stalest one. Without per-store databases every store shares
// one source.
func ReadFreshness(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, tenants []int32) (Freshness, error) {
	rows, err := q.Query(ctx, `SELECT applied_lsn::text, applied_commit_time, heartbeat_time FROM cdc_sources
		WHERE tenant_id = ANY($1) OR NOT EXISTS (SELECT 1 FROM cdc_sources WHERE tenant_id IS NOT NULL)`, tenants)
	if err != nil {
		return Freshness{}, err
	}
	defer rows.Close()
	var oldest Freshness
	first := true
	for rows.Next() {
		var f Freshness
		var commit, hb *time.Time
		if err := rows.Scan(&f.LSN, &commit, &hb); err != nil {
			return Freshness{}, err
		}
		if commit != nil {
			f.CommitTime = *commit
		}
		if hb != nil {
			f.Heartbeat = *hb
		}
		if first || f.AsOf().Before(oldest.AsOf()) {
			oldest, first = f, false
		}
	}
	return oldest, rows.Err()
}

// ErrNoTenantContext means a read ran without its tenant setting. Row-level
// security returns no rows in that case (it fails closed); Cuboid also
// refuses the answer and records a security event.
var ErrNoTenantContext = errors.New("tenant context missing on an aggregate-store read")

// ReadOptions alter a read for the leak drill only.
type ReadOptions struct {
	SkipTenantContext bool // drill C: behave as if set_config were forgotten
}

// ReadTx runs fn in a read-only REPEATABLE READ transaction whose RLS
// setting names exactly the given tenants. The setting is transaction-local
// (set_config(..., true)), so a pooled connection can never carry one
// request's tenants into another request: the INC-02 class of bug.
func (s *Store) ReadTx(ctx context.Context, tenants []int32, opt ReadOptions, fn func(tx pgx.Tx, f Freshness) error) error {
	tx, err := s.R.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	setting := TenantSetting(tenants)
	if opt.SkipTenantContext {
		setting = ""
	}
	var got string
	if err := tx.QueryRow(ctx, `SELECT set_config('cuboid.tenant_ids', $1, true)`, setting).Scan(&got); err != nil {
		return err
	}
	if got == "" {
		return ErrNoTenantContext
	}
	f, err := ReadFreshness(ctx, tx, tenants)
	if err != nil {
		return err
	}
	if err := fn(tx, f); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TenantSetting renders a tenant set for cuboid.tenant_ids.
func TenantSetting(tenants []int32) string {
	parts := make([]string, len(tenants))
	for i, t := range tenants {
		parts[i] = strconv.Itoa(int(t))
	}
	return strings.Join(parts, ",")
}

// RLSPolicy is the row-level security predicate on every tenant table.
// It fails closed: an unset or empty setting matches no rows.
const RLSPolicy = `tenant_id = ANY (string_to_array(current_setting('cuboid.tenant_ids', true), ',')::int[])`

// QuoteIdent quotes an identifier. Every identifier in generated SQL comes
// from validated config or the catalog, and is quoted regardless.
func QuoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }
