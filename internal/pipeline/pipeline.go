// Package pipeline connects the change stream to the engine and the flusher.
// One goroutine owns the engine, the pending delta and the flush cadence, so
// nothing in the write path needs a lock. Other goroutines talk to it through
// Do, which runs a function on that goroutine between messages.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"

	"cuboid/internal/cdc"
	"cuboid/internal/engine"
	"cuboid/internal/flush"
)

// FenceReached is sent to a verifier when the stream reaches its fence and
// the aggregate store holds exactly the transactions committed before it.
// The verifier must take its read snapshot and then close Release; the next
// flush waits for that (or for a short timeout).
type FenceReached struct {
	LSN     pglogrepl.LSN
	Release chan struct{}
	Busy    bool // a rebuild holds the flusher: no flush boundary is possible now
}

// Pipeline is the stream handler.
type Pipeline struct {
	Engine   *engine.Engine
	Flusher  *flush.Flusher
	Interval time.Duration
	MaxCells int
	Log      *slog.Logger
	// OnFlush runs after every committed flush (cache versions, live push, metrics).
	OnFlush func(res *flush.Result)
	// OnTruncate runs when a published table is truncated.
	OnTruncate func(tables []string)
	// OnEvent records security events and alarms.
	OnEvent func(kind, severity, detail string)

	delta      *engine.Delta
	applied    atomic.Uint64 // LSN committed in AGG
	pending    pglogrepl.LSN // LSN processed into delta
	commitTime time.Time
	heartbeat  time.Time
	lastFlush  time.Time

	ctrl   chan func()
	paused bool

	mu      sync.Mutex
	fences  map[string]chan FenceReached
	ring    *Ring
	logging *engine.Delta // an online rebuild's delta log (cells only), nil when off
	logMu   sync.Mutex

	appliedCommit atomic.Pointer[time.Time]
	appliedHB     atomic.Pointer[time.Time]
	Flushes       atomic.Int64
	TxnsApplied   atomic.Int64
	LastFlushMS   atomic.Int64
}

// New returns a pipeline resuming after applied.
func New(e *engine.Engine, f *flush.Flusher, applied pglogrepl.LSN, log *slog.Logger) *Pipeline {
	p := &Pipeline{
		Engine: e, Flusher: f, Interval: time.Second, MaxCells: 200000, Log: log,
		delta: engine.NewDelta(), pending: applied, ctrl: make(chan func(), 64),
		fences: map[string]chan FenceReached{}, ring: NewRing(20 * time.Second),
	}
	p.applied.Store(uint64(applied))
	return p
}

// Applied is the last LSN committed in the aggregate store.
func (p *Pipeline) Applied() pglogrepl.LSN { return pglogrepl.LSN(p.applied.Load()) }

// AppliedTimes returns the commit time and heartbeat time of the last flush.
func (p *Pipeline) AppliedTimes() (commit, heartbeat time.Time) {
	if t := p.appliedCommit.Load(); t != nil {
		commit = *t
	}
	if t := p.appliedHB.Load(); t != nil {
		heartbeat = *t
	}
	return
}

// Ring exposes recent transactions for fenced verification.
func (p *Pipeline) Ring() *Ring { return p.ring }

// Confirmed implements cdc.Handler.
func (p *Pipeline) Confirmed() pglogrepl.LSN { return p.Applied() }

// Do runs fn on the pipeline goroutine and waits for it.
func (p *Pipeline) Do(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	select {
	case p.ctrl <- func() { done <- fn() }:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pause flushes what is pending and stops flushing until Resume. The stream
// keeps being read into the delta, so nothing is lost. atBoundary, if given,
// runs in the same step: no transaction can slip between the flush and it, so
// a config switched there applies to exactly the transactions after the flush.
func (p *Pipeline) Pause(ctx context.Context, atBoundary func()) (pglogrepl.LSN, error) {
	var lsn pglogrepl.LSN
	err := p.Do(ctx, func() error {
		if err := p.flush(ctx); err != nil {
			return err
		}
		p.paused = true
		lsn = p.Applied()
		if atBoundary != nil {
			atBoundary()
		}
		return nil
	})
	return lsn, err
}

// Resume restarts flushing.
func (p *Pipeline) Resume(ctx context.Context) error {
	return p.Do(ctx, func() error { p.paused = false; return nil })
}

// StartDeltaLog begins recording every flushed cell delta (online rebuild).
func (p *Pipeline) StartDeltaLog() {
	p.logMu.Lock()
	p.logging = engine.NewDelta()
	p.logMu.Unlock()
}

// TakeDeltaLog returns the deltas recorded since the last take, and keeps
// logging if keep is true.
func (p *Pipeline) TakeDeltaLog(keep bool) map[engine.CellKey][]int64 {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.logging == nil {
		return nil
	}
	out := p.logging.NonZeroCells()
	if keep {
		p.logging = engine.NewDelta()
	} else {
		p.logging = nil
	}
	return out
}

// AwaitFence registers interest in a fence token before it is emitted.
func (p *Pipeline) AwaitFence(token string) <-chan FenceReached {
	ch := make(chan FenceReached, 1)
	p.mu.Lock()
	p.fences[token] = ch
	p.mu.Unlock()
	return ch
}

// CancelFence drops a fence registration.
func (p *Pipeline) CancelFence(token string) {
	p.mu.Lock()
	delete(p.fences, token)
	p.mu.Unlock()
}

// Txn implements cdc.Handler.
func (p *Pipeline) Txn(ctx context.Context, txn *engine.Txn) error {
	if alreadyApplied(txn.CommitLSN, p.applied.Load()) {
		return nil // redelivered after a restart: already in AGG
	}
	if err := p.Engine.Apply(txn, p.delta); err != nil {
		return fmt.Errorf("apply transaction %d at %s: %w", txn.Xid, pglogrepl.LSN(txn.CommitLSN), err)
	}
	p.delta.Txns++
	p.ring.Add(txn)
	p.pending = pglogrepl.LSN(txn.CommitLSN)
	p.commitTime = txn.CommitTime
	p.TxnsApplied.Add(1)
	if len(p.delta.Cells) > p.MaxCells && !p.paused {
		return p.flush(ctx)
	}
	return nil
}

// messagePosition is the applied position a logical decoding message allows.
//
// Postgres reports a message's LSN as the END of its WAL record, and a
// commit's LSN as the START of its record. A commit written directly after a
// message therefore has the same LSN as the message. Recording the message at
// its own LSN would make that new transaction look already applied, and the
// redelivery check in Txn would silently drop it. One less is still past every
// earlier commit, and can never reach a later one.
func messagePosition(lsn pglogrepl.LSN) pglogrepl.LSN { return lsn - 1 }

// alreadyApplied is the redelivery check: after a restart Postgres may resend
// transactions at or below the committed position.
func alreadyApplied(commitLSN uint64, applied uint64) bool { return commitLSN <= applied }

// Message implements cdc.Handler.
func (p *Pipeline) Message(ctx context.Context, prefix, content string, lsn pglogrepl.LSN) error {
	pos := messagePosition(lsn)
	if uint64(pos) <= p.applied.Load() {
		return nil
	}
	switch prefix {
	case cdc.PrefixHeartbeat:
		if t, err := time.Parse(time.RFC3339Nano, content); err == nil {
			p.heartbeat = t
		}
		if pos > p.pending {
			p.pending = pos
		}
	case cdc.PrefixFence:
		if pos > p.pending {
			p.pending = pos
		}
		p.mu.Lock()
		ch := p.fences[content]
		delete(p.fences, content)
		p.mu.Unlock()
		if ch == nil {
			return nil
		}
		// While paused for a rebuild, nothing may flush: the rebuild took its
		// snapshot at the last flush and logs only later ones, so a flush here
		// would be in neither. The check is asked to try again.
		if p.paused {
			ch <- FenceReached{LSN: lsn, Busy: true}
			return nil
		}
		// Force a flush boundary exactly at the fence: AGG now holds every
		// transaction whose commit precedes the fence and none after it.
		if err := p.flush(ctx); err != nil {
			return err
		}
		release := make(chan struct{})
		ch <- FenceReached{LSN: lsn, Release: release}
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Truncate implements cdc.Handler.
func (p *Pipeline) Truncate(ctx context.Context, tables []string, lsn pglogrepl.LSN) error {
	if p.OnEvent != nil {
		p.OnEvent("truncate", "P1", fmt.Sprintf("TRUNCATE of %v on the main database: rollups are invalid and are being rebuilt from the main database", tables))
	}
	if p.OnTruncate != nil {
		p.OnTruncate(tables)
	}
	return cdc.ErrTruncated
}

// Idle implements cdc.Handler.
func (p *Pipeline) Idle(ctx context.Context) error {
	for {
		select {
		case fn := <-p.ctrl:
			fn()
			continue
		default:
		}
		break
	}
	if p.paused {
		return nil
	}
	if time.Since(p.lastFlush) >= p.Interval && (uint64(p.pending) > p.applied.Load() || !p.delta.Empty()) {
		return p.flush(ctx)
	}
	return nil
}

// Flush forces a flush (used by tests and before shutdown).
func (p *Pipeline) Flush(ctx context.Context) error {
	return p.Do(ctx, func() error { return p.flush(ctx) })
}

func (p *Pipeline) flush(ctx context.Context) error {
	p.lastFlush = time.Now()
	if uint64(p.pending) <= p.applied.Load() && p.delta.Empty() {
		return nil
	}
	d := p.delta
	pos := flush.Position{LSN: p.pending.String(), CommitTime: p.commitTime, Heartbeat: p.heartbeat}
	res, err := p.Flusher.Flush(ctx, p.Engine.Config(), d, pos)
	if err != nil {
		if p.OnEvent != nil {
			p.OnEvent("flush_failed", "P1", err.Error())
		}
		return fmt.Errorf("flush at %s: %w", p.pending, err)
	}
	p.delta = engine.NewDelta()
	p.applied.Store(uint64(p.pending))
	ct, hb := p.commitTime, p.heartbeat
	p.appliedCommit.Store(&ct)
	p.appliedHB.Store(&hb)
	p.Flushes.Add(1)
	p.LastFlushMS.Store(res.Duration.Milliseconds())
	p.logMu.Lock()
	if p.logging != nil {
		p.logging.MergeCells(res.CellDeltas)
	}
	p.logMu.Unlock()
	if d.FutureTimestamps > 0 && p.OnEvent != nil {
		p.OnEvent("future_timestamp", "P3", fmt.Sprintf("%d rows were created more than 10 minutes in the future; they are aggregated and flagged", d.FutureTimestamps))
	}
	for _, r := range res.Rebuilt {
		p.Log.Info("dimension change: rebuilt from the mirror", "scope", r)
	}
	if p.OnFlush != nil {
		p.OnFlush(res)
	}
	return nil
}

// ErrStopped is returned by Run when the pipeline is asked to stop.
var ErrStopped = errors.New("pipeline stopped")
