// Package audit records every data access (who, which stores, what query,
// how big the answer was, cache status) and every security event. Writes are
// batched off the request path with COPY.
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Access is one data access.
type Access struct {
	At         time.Time
	Scope      string
	ScopeID    int32
	Tenants    []int32
	KeyPrefix  string
	Endpoint   string
	Query      string
	Status     int
	ResultRows int
	Cache      string
	DurationMS float32
}

// Event is one security event.
type Event struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Tenant    int32     `json:"tenant_id,omitempty"`
	KeyPrefix string    `json:"key_prefix,omitempty"`
	Detail    string    `json:"detail"`
}

// eventsPerSecond caps the events stored per kind in one second. A drill
// produces thousands of identical events: the first few say what happened and
// a summary row says how many more there were, so the count is never lost.
const eventsPerSecond = 5

// Log buffers accesses and events and writes them every second.
type Log struct {
	pool   *pgxpool.Pool
	log    *slog.Logger
	mu     sync.Mutex
	acc    []Access
	events []Event
	recent []Event
	second int64
	seen   map[string]int
	sev    map[string]string
	Drops  int64
	// Retention is how long rows are kept; MaxAccessBytes and MaxEventBytes
	// bound the tables, so a long load test cannot fill a laptop's disk.
	Retention      time.Duration
	MaxAccessBytes int64
	MaxEventBytes  int64
}

// New returns an audit log writing to pool.
func New(pool *pgxpool.Pool, log *slog.Logger) *Log {
	return &Log{pool: pool, log: log, seen: map[string]int{}, sev: map[string]string{}, Retention: 365 * 24 * time.Hour, MaxAccessBytes: 512 << 20, MaxEventBytes: 64 << 20}
}

// Access records a data access.
func (l *Log) Access(a Access) {
	l.mu.Lock()
	if len(l.acc) < 200000 {
		l.acc = append(l.acc, a)
	} else {
		l.Drops++
	}
	l.mu.Unlock()
}

// Event records a security event and keeps it in the recent list.
func (l *Log) Event(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	l.mu.Lock()
	l.rollSecond(e.At.Unix())
	l.seen[e.Kind]++
	l.sev[e.Kind] = e.Severity
	keep := l.seen[e.Kind] <= eventsPerSecond
	if keep {
		l.keep(e)
	}
	l.mu.Unlock()
	if keep {
		l.log.Warn("security event", "kind", e.Kind, "severity", e.Severity, "tenant", e.Tenant, "detail", e.Detail)
	}
}

// keep stores one event. The caller holds mu.
func (l *Log) keep(e Event) {
	l.events = append(l.events, e)
	l.recent = append(l.recent, e)
	if len(l.recent) > 200 {
		l.recent = l.recent[len(l.recent)-200:]
	}
}

// rollSecond starts a new second, first summarising what the last one dropped.
// The caller holds mu.
func (l *Log) rollSecond(now int64) {
	if now == l.second {
		return
	}
	for kind, n := range l.seen {
		if n > eventsPerSecond {
			l.keep(Event{At: time.Unix(l.second, 0), Kind: kind, Severity: l.sev[kind],
				Detail: fmt.Sprintf("%d more of this event in this second, not stored one by one", n-eventsPerSecond)})
		}
	}
	clear(l.seen)
	l.second = now
}

// Recent returns the latest security events, newest last.
func (l *Log) Recent() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.recent...)
}

// Run writes batches until ctx ends.
func (l *Log) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	prune := time.NewTicker(10 * time.Minute)
	defer prune.Stop()
	l.prune(ctx)
	for {
		select {
		case <-ctx.Done():
			l.write(context.Background())
			return
		case <-t.C:
			l.mu.Lock()
			l.rollSecond(time.Now().Unix())
			l.mu.Unlock()
			l.write(ctx)
		case <-prune.C:
			l.prune(ctx)
		}
	}
}

// prune removes rows past retention, then the oldest fifth of a table over its
// size bound. The time column keeps both readable by anyone auditing later.
func (l *Log) prune(ctx context.Context) {
	for _, t := range []struct {
		table string
		max   int64
	}{{"audit_log", l.MaxAccessBytes}, {"security_events", l.MaxEventBytes}} {
		if _, err := l.pool.Exec(ctx, "DELETE FROM "+t.table+" WHERE at < now() - $1::interval", fmt.Sprintf("%d seconds", int64(l.Retention.Seconds()))); err != nil {
			l.log.Warn("prune audit rows", "table", t.table, "err", err)
			continue
		}
		var size int64
		if err := l.pool.QueryRow(ctx, "SELECT pg_total_relation_size($1::regclass)", t.table).Scan(&size); err != nil || size <= t.max {
			continue
		}
		tag, err := l.pool.Exec(ctx, "DELETE FROM "+t.table+" WHERE at < (SELECT min(at) + (max(at) - min(at)) / 5 FROM "+t.table+")")
		if err != nil {
			l.log.Warn("prune audit rows", "table", t.table, "err", err)
			continue
		}
		l.log.Info("pruned oldest audit rows over the size bound", "table", t.table, "rows", tag.RowsAffected())
	}
}

func (l *Log) write(ctx context.Context) {
	l.mu.Lock()
	acc, ev := l.acc, l.events
	l.acc, l.events = nil, nil
	l.mu.Unlock()
	if len(acc) > 0 {
		rows := make([][]any, len(acc))
		for i, a := range acc {
			rows[i] = []any{a.At, a.Scope, a.ScopeID, a.Tenants, a.KeyPrefix, a.Endpoint, a.Query, a.Status, a.ResultRows, a.Cache, a.DurationMS}
		}
		_, err := l.pool.CopyFrom(ctx, pgx.Identifier{"audit_log"},
			[]string{"at", "scope", "scope_id", "tenant_ids", "key_prefix", "endpoint", "query", "status", "result_rows", "cache", "duration_ms"},
			pgx.CopyFromRows(rows))
		if err != nil {
			l.log.Error("write audit log", "err", err, "rows", len(rows))
		}
	}
	for _, e := range ev {
		if _, err := l.pool.Exec(ctx, `INSERT INTO security_events (at, kind, severity, tenant_id, key_prefix, detail) VALUES ($1, $2, $3, $4, $5, $6)`,
			e.At, e.Kind, e.Severity, nullInt(e.Tenant), e.KeyPrefix, e.Detail); err != nil {
			l.log.Error("write security event", "err", err)
		}
	}
}

func nullInt(v int32) any {
	if v == 0 {
		return nil
	}
	return v
}
