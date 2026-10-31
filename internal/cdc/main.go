package cdc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AllowedMainRoles are the only roles Cuboid may hold on the main database
// (invariant I1): the change stream and the budgeted verifier. Cuboid never
// holds a main-database connection that could serve a dashboard query.
var AllowedMainRoles = map[string]bool{"cuboid_cdc": true, "cuboid_verify": true}

// CheckRole connects with dsn and refuses roles outside AllowedMainRoles.
func CheckRole(ctx context.Context, dsn string, want string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse main DSN: %w", err)
	}
	delete(cfg.RuntimeParams, "replication")
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect to main database as %s: %w", cfg.User, err)
	}
	defer conn.Close(ctx)
	var role string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		return err
	}
	if !AllowedMainRoles[role] || role != want {
		return fmt.Errorf("refusing to start: a main-database DSN connects as %q, but Cuboid may only hold %s there (the query path never touches the main database)", role, want)
	}
	return nil
}

// ConnectReplication opens a replication connection (replication=database).
func ConnectReplication(ctx context.Context, dsn string) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["replication"] = "database"
	cfg.RuntimeParams["application_name"] = "cuboid-cdc"
	return pgconn.ConnectConfig(ctx, cfg)
}

// ConnectPlain opens an ordinary connection with a replication DSN's role.
func ConnectPlain(ctx context.Context, dsn, app string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	delete(cfg.RuntimeParams, "replication")
	cfg.RuntimeParams["application_name"] = app
	return pgx.ConnectConfig(ctx, cfg)
}

// Emitter writes heartbeats and fences into the WAL with
// pg_logical_emit_message (non-transactional: they enter the WAL at once).
type Emitter struct {
	mu   sync.Mutex
	conn *pgx.Conn
	dsn  string
}

// NewEmitter returns an emitter using an ordinary cuboid_cdc connection.
func NewEmitter(dsn string) *Emitter { return &Emitter{dsn: dsn} }

// Emit writes a message and returns its LSN.
func (e *Emitter) Emit(ctx context.Context, prefix, content string) (pglogrepl.LSN, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil || e.conn.IsClosed() {
		c, err := ConnectPlain(ctx, e.dsn, "cuboid-emitter")
		if err != nil {
			return 0, err
		}
		e.conn = c
	}
	var lsn string
	err := e.conn.QueryRow(ctx, "SELECT pg_logical_emit_message(false, $1, $2)::text", prefix, content).Scan(&lsn)
	if err != nil {
		e.conn.Close(context.Background())
		e.conn = nil
		return 0, fmt.Errorf("emit %s: %w", prefix, err)
	}
	return pglogrepl.ParseLSN(lsn)
}

// Heartbeats emits a heartbeat every interval until ctx ends. Each carries
// its send time, so applying it measures true end-to-end freshness, and it
// keeps the slot advancing when the main database is idle.
func (e *Emitter) Heartbeats(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := e.Emit(ctx, PrefixHeartbeat, time.Now().UTC().Format(time.RFC3339Nano)); err != nil && ctx.Err() == nil {
				onErr(err)
			}
		}
	}
}

// Loading reports whether a bulk data load is running on the main database.
func (e *Emitter) Loading(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil || e.conn.IsClosed() {
		c, err := ConnectPlain(ctx, e.dsn, "cuboid-emitter")
		if err != nil {
			return false
		}
		e.conn = c
	}
	var loading bool
	if err := e.conn.QueryRow(ctx, "SELECT loading FROM seed_status").Scan(&loading); err != nil {
		return false // no such table on a database Cuboid did not seed
	}
	return loading
}

// SlotHealth is what pg_replication_slots says about Cuboid's slot.
type SlotHealth struct {
	Exists       bool
	Active       bool
	WALStatus    string
	RetainedWAL  int64 // bytes between restart_lsn and the current WAL position
	LagBytes     int64 // bytes between confirmed_flush_lsn and the current WAL position
	SafeWALBytes *int64
	ConfirmedLSN string
}

// Health reads the slot's state.
func (e *Emitter) Health(ctx context.Context, slot string) (SlotHealth, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var h SlotHealth
	if e.conn == nil || e.conn.IsClosed() {
		c, err := ConnectPlain(ctx, e.dsn, "cuboid-emitter")
		if err != nil {
			return h, err
		}
		e.conn = c
	}
	err := e.conn.QueryRow(ctx, `SELECT active, coalesce(wal_status, ''),
			coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0)::bigint,
			coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn), 0)::bigint,
			safe_wal_size, coalesce(confirmed_flush_lsn::text, '')
		FROM pg_replication_slots WHERE slot_name = $1`, slot).
		Scan(&h.Active, &h.WALStatus, &h.RetainedWAL, &h.LagBytes, &h.SafeWALBytes, &h.ConfirmedLSN)
	if err == pgx.ErrNoRows {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	h.Exists = true
	return h, nil
}

// DropSlot drops a slot if it exists and is not in use.
func (e *Emitter) DropSlot(ctx context.Context, slot string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil || e.conn.IsClosed() {
		c, err := ConnectPlain(ctx, e.dsn, "cuboid-emitter")
		if err != nil {
			return err
		}
		e.conn = c
	}
	for i := 0; i < 20; i++ {
		_, err := e.conn.Exec(ctx, "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1", slot)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "is active") {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("slot %s is still active", slot)
}
