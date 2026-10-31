// Package audit records every data access (who, which stores, what query,
// how big the answer was, cache status) and every security event. Writes are
// batched off the request path with COPY.
package audit

import (
	"context"
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

// Log buffers accesses and events and writes them every second.
type Log struct {
	pool   *pgxpool.Pool
	log    *slog.Logger
	mu     sync.Mutex
	acc    []Access
	events []Event
	recent []Event
	Drops  int64
}

// New returns an audit log writing to pool.
func New(pool *pgxpool.Pool, log *slog.Logger) *Log { return &Log{pool: pool, log: log} }

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
	l.events = append(l.events, e)
	l.recent = append(l.recent, e)
	if len(l.recent) > 200 {
		l.recent = l.recent[len(l.recent)-200:]
	}
	l.mu.Unlock()
	l.log.Warn("security event", "kind", e.Kind, "severity", e.Severity, "tenant", e.Tenant, "detail", e.Detail)
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
	for {
		select {
		case <-ctx.Done():
			l.write(context.Background())
			return
		case <-t.C:
			l.write(ctx)
		}
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
