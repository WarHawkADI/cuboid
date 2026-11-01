// Package pipeline connects the change streams to the engine and the flusher.
// Each source database (the platform's, and one per store) has its own stream;
// all of them hand whole transactions to one goroutine, which owns the engine,
// the pending delta and the flush cadence, so nothing in the write path needs a
// lock. Each source keeps its own applied position, and every flush commits
// the positions of the sources it covers in the same transaction as the data.
// Other goroutines talk to the pipeline through Do, which runs a function on
// its goroutine between transactions.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"

	"cuboid/internal/cdc"
	"cuboid/internal/engine"
	"cuboid/internal/flush"
)

// FenceReached is sent to a verifier when a source's stream reaches its fence.
// That stream then holds: none of its later transactions is applied until
// Release is closed (or a timeout passes), so a flush now leaves the aggregate
// store with exactly that source's transactions committed before the fence.
type FenceReached struct {
	LSN     pglogrepl.LSN
	Release chan struct{}
	Busy    bool // a rebuild holds the flusher: no flush boundary is possible now
}

// sourceState is one source database's position. The pipeline goroutine owns
// it; readers use the published copy.
type sourceState struct {
	tenant     int32 // the store whose database it is; 0 when rows carry their own
	pending    pglogrepl.LSN
	commitTime time.Time
	heartbeat  time.Time
	dirty      bool // pending or the times moved since the last flush
	pub        *published
}

type published struct {
	applied atomic.Uint64
	commit  atomic.Pointer[time.Time]
	hb      atomic.Pointer[time.Time]
}

// event is one call from a stream, answered on reply.
type event struct {
	source   string
	txn      *engine.Txn
	prefix   string
	content  string
	lsn      pglogrepl.LSN
	message  bool
	truncate []string
	reply    chan error
}

// Pipeline is the handler of every source's stream.
type Pipeline struct {
	Engine   *engine.Engine
	Flusher  *flush.Flusher
	Interval time.Duration
	MaxCells int
	Log      *slog.Logger
	// TenantColumn is the column that names a row's store. Rows from a store's
	// own database are stamped with that store, whatever they say.
	TenantColumn string
	// OnFlush runs after every committed flush (cache versions, live push, metrics).
	OnFlush func(res *flush.Result)
	// OnTruncate runs when a published table of a source is truncated.
	OnTruncate func(source string, tables []string)
	// OnEvent records security events and alarms.
	OnEvent func(kind, severity, detail string)

	delta     *engine.Delta
	sources   map[string]*sourceState // pipeline goroutine only
	pub       sync.Map                // source -> *published, for readers
	lastFlush time.Time
	failing   bool

	events chan event
	ctrl   chan func()
	paused bool

	mu      sync.Mutex
	fences  map[string]chan FenceReached
	ring    *Ring
	logging *engine.Delta // an online rebuild's delta log (cells only), nil when off
	logMu   sync.Mutex

	Flushes     atomic.Int64
	TxnsApplied atomic.Int64
	LastFlushMS atomic.Int64
}

// New returns a pipeline with no sources yet.
func New(e *engine.Engine, f *flush.Flusher, log *slog.Logger) *Pipeline {
	return &Pipeline{
		Engine: e, Flusher: f, Interval: time.Second, MaxCells: 200000, Log: log,
		delta: engine.NewDelta(), sources: map[string]*sourceState{},
		events: make(chan event), ctrl: make(chan func(), 64),
		fences: map[string]chan FenceReached{}, ring: NewRing(20 * time.Second),
	}
}

// Source registers a source database, or moves an existing one, at its
// position committed in the aggregate store. A store's source moves after
// that store alone was reloaded from a new snapshot.
func (p *Pipeline) Source(ctx context.Context, name string, tenant int32, applied pglogrepl.LSN, commit, heartbeat time.Time) error {
	return p.Do(ctx, func() error {
		pub, _ := p.pub.LoadOrStore(name, &published{})
		st := &sourceState{tenant: tenant, pending: applied, commitTime: commit, heartbeat: heartbeat, pub: pub.(*published)}
		st.pub.applied.Store(uint64(applied))
		st.pub.commit.Store(&commit)
		st.pub.hb.Store(&heartbeat)
		p.sources[name] = st
		return nil
	})
}

// Applied is the last position of a source committed in the aggregate store.
func (p *Pipeline) Applied(source string) pglogrepl.LSN {
	if v, ok := p.pub.Load(source); ok {
		return pglogrepl.LSN(v.(*published).applied.Load())
	}
	return 0
}

// AppliedTimes returns the commit and heartbeat time of a source's last flush.
func (p *Pipeline) AppliedTimes(source string) (commit, heartbeat time.Time) {
	v, ok := p.pub.Load(source)
	if !ok {
		return
	}
	pub := v.(*published)
	if t := pub.commit.Load(); t != nil {
		commit = *t
	}
	if t := pub.hb.Load(); t != nil {
		heartbeat = *t
	}
	return
}

// Ring exposes recent transactions for fenced verification.
func (p *Pipeline) Ring() *Ring { return p.ring }

// Handler returns the stream handler of one source.
func (p *Pipeline) Handler(source string) cdc.Handler { return handler{p, source} }

type handler struct {
	p      *Pipeline
	source string
}

func (h handler) Txn(ctx context.Context, txn *engine.Txn) error {
	txn.Source = h.source
	return h.p.send(ctx, event{source: h.source, txn: txn})
}

func (h handler) Message(ctx context.Context, prefix, content string, lsn pglogrepl.LSN) error {
	return h.p.send(ctx, event{source: h.source, message: true, prefix: prefix, content: content, lsn: lsn})
}

func (h handler) Truncate(ctx context.Context, tables []string, lsn pglogrepl.LSN) error {
	return h.p.send(ctx, event{source: h.source, truncate: tables})
}

// Idle has nothing to do: the pipeline keeps its own flush cadence.
func (h handler) Idle(ctx context.Context) error { return nil }

// Confirmed is the source's position committed in the aggregate store: the
// only position ever reported to its database.
func (h handler) Confirmed() pglogrepl.LSN { return h.p.Applied(h.source) }

// send hands an event to the pipeline goroutine and waits for its answer, so
// a stream never runs ahead of what the pipeline has taken.
func (p *Pipeline) send(ctx context.Context, ev event) error {
	ev.reply = make(chan error, 1)
	select {
	case p.events <- ev:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ev.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is the pipeline goroutine. It returns when ctx ends.
func (p *Pipeline) Run(ctx context.Context) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fn := <-p.ctrl:
			fn()
		case ev := <-p.events:
			if held, err := p.handle(ctx, ev); !held {
				ev.reply <- err
			}
		case <-tick.C:
			p.idle(ctx)
		}
	}
}

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

// Pause flushes what is pending and stops flushing until Resume. The streams
// keep being read into the delta, so nothing is lost. atBoundary, if given,
// runs in the same step: no transaction can slip between the flush and it, so
// a config switched there applies to exactly the transactions after the flush.
func (p *Pipeline) Pause(ctx context.Context, atBoundary func()) error {
	return p.Do(ctx, func() error {
		if err := p.flush(ctx); err != nil {
			return err
		}
		p.paused = true
		if atBoundary != nil {
			atBoundary()
		}
		return nil
	})
}

// Resume restarts flushing.
func (p *Pipeline) Resume(ctx context.Context) error {
	return p.Do(ctx, func() error { p.paused = false; return nil })
}

// Flush forces a flush. When it returns, every transaction the pipeline has
// taken from any source is in the aggregate store.
func (p *Pipeline) Flush(ctx context.Context) error {
	return p.Do(ctx, func() error { return p.flush(ctx) })
}

// ForgetTenant drops a store's cached dimension rows, after it was reloaded.
func (p *Pipeline) ForgetTenant(ctx context.Context, tenant int32) error {
	return p.Do(ctx, func() error { p.Engine.ForgetTenant(tenant); return nil })
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

// handle processes one event. held means the stream must wait: its answer is
// sent later, when a verifier releases the fence it reached.
func (p *Pipeline) handle(ctx context.Context, ev event) (held bool, err error) {
	st := p.sources[ev.source]
	if st == nil {
		return false, fmt.Errorf("source %s is not registered with the pipeline", ev.source)
	}
	switch {
	case ev.txn != nil:
		return false, p.txn(ctx, ev.source, st, ev.txn)
	case ev.message:
		return p.message(ctx, st, ev)
	default:
		if p.OnEvent != nil {
			p.OnEvent("truncate", "P1", fmt.Sprintf("TRUNCATE of %v in %s: its numbers are invalid and are being reloaded from that database", ev.truncate, ev.source))
		}
		if p.OnTruncate != nil {
			p.OnTruncate(ev.source, ev.truncate)
		}
		return false, cdc.ErrTruncated
	}
}

func (p *Pipeline) txn(ctx context.Context, source string, st *sourceState, txn *engine.Txn) error {
	// A transaction at or before the source's pending position is already in
	// the delta or in the aggregate store: a stream that reconnects starts
	// from the committed position and sends it again.
	if alreadyApplied(txn.CommitLSN, uint64(st.pending)) {
		return nil
	}
	if st.tenant != 0 {
		p.stamp(source, st.tenant, txn)
	}
	if err := p.Engine.Apply(txn, p.delta); err != nil {
		return fmt.Errorf("apply transaction %d at %s from %s: %w", txn.Xid, pglogrepl.LSN(txn.CommitLSN), source, err)
	}
	p.delta.Txns++
	p.ring.Add(txn)
	st.pending = pglogrepl.LSN(txn.CommitLSN)
	st.commitTime = txn.CommitTime
	st.dirty = true
	p.TxnsApplied.Add(1)
	if len(p.delta.Cells) > p.MaxCells && !p.paused {
		return p.flush(ctx)
	}
	return nil
}

// stamp attributes every row of a store's database to that store. The
// database a row sits in decides whose it is; a row naming another store is
// counted where it lives, and reported.
func (p *Pipeline) stamp(source string, tenant int32, txn *engine.Txn) {
	want := strconv.Itoa(int(tenant))
	for i := range txn.Changes {
		for _, r := range []engine.Row{txn.Changes[i].Old, txn.Changes[i].New} {
			if r == nil {
				continue
			}
			if got, ok := r[p.TenantColumn]; ok && got != want && p.OnEvent != nil {
				p.OnEvent("foreign_row", "P1", fmt.Sprintf("a row in %s, the database of store %d, names store %s in %s; it is counted for store %d", source, tenant, got, txn.Changes[i].Table, tenant))
			}
			r[p.TenantColumn] = want
		}
	}
}

// messagePosition is the applied position a logical decoding message allows.
//
// Postgres reports a message's LSN as the END of its WAL record, and a
// commit's LSN as the START of its record. A commit written directly after a
// message therefore has the same LSN as the message. Recording the message at
// its own LSN would make that new transaction look already applied, and the
// redelivery check in txn would silently drop it. One less is still past every
// earlier commit, and can never reach a later one.
func messagePosition(lsn pglogrepl.LSN) pglogrepl.LSN { return lsn - 1 }

// alreadyApplied is the redelivery check: after a reconnect or a restart
// Postgres may resend transactions at or below the position taken.
func alreadyApplied(commitLSN uint64, taken uint64) bool { return commitLSN <= taken }

func (p *Pipeline) message(ctx context.Context, st *sourceState, ev event) (bool, error) {
	pos := messagePosition(ev.lsn)
	if pos <= st.pending {
		return false, nil
	}
	switch ev.prefix {
	case cdc.PrefixHeartbeat:
		if t, err := time.Parse(time.RFC3339Nano, ev.content); err == nil {
			st.heartbeat = t
		}
		st.pending, st.dirty = pos, true
	case cdc.PrefixFence:
		st.pending, st.dirty = pos, true
		p.mu.Lock()
		ch := p.fences[ev.content]
		delete(p.fences, ev.content)
		p.mu.Unlock()
		if ch == nil {
			return false, nil
		}
		// While paused for a rebuild, nothing may flush: the rebuild took its
		// snapshot at the last flush and logs only later ones, so a flush the
		// verifier forced would be in neither. The check is asked to try again.
		if p.paused {
			ch <- FenceReached{LSN: ev.lsn, Busy: true}
			return false, nil
		}
		// Hold this source's stream at its fence until the verifier has its
		// snapshot. Other sources keep streaming.
		release := make(chan struct{})
		ch <- FenceReached{LSN: ev.lsn, Release: release}
		go func() {
			select {
			case <-release:
			case <-time.After(15 * time.Second):
			case <-ctx.Done():
			}
			ev.reply <- nil
		}()
		return true, nil
	}
	return false, nil
}

func (p *Pipeline) idle(ctx context.Context) {
	if p.paused || time.Since(p.lastFlush) < p.Interval {
		return
	}
	if p.delta.Empty() && !p.anyDirty() {
		return
	}
	if err := p.flush(ctx); err != nil && p.Log != nil {
		p.Log.Error("flush failed; the delta is kept and the flush retried", "err", err)
	}
}

func (p *Pipeline) anyDirty() bool {
	for _, st := range p.sources {
		if st.dirty {
			return true
		}
	}
	return false
}

// flush writes the delta and every moved source position in one transaction.
// On failure the delta and positions are kept: nothing was confirmed to any
// source database, so the next flush writes them all.
func (p *Pipeline) flush(ctx context.Context) error {
	p.lastFlush = time.Now()
	if p.delta.Empty() && !p.anyDirty() {
		return nil
	}
	var names []string
	for name, st := range p.sources {
		if st.dirty {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	positions := make([]flush.Position, len(names))
	for i, name := range names {
		st := p.sources[name]
		positions[i] = flush.Position{Source: name, LSN: st.pending.String(), CommitTime: st.commitTime, Heartbeat: st.heartbeat}
	}
	d := p.delta
	res, err := p.Flusher.Flush(ctx, p.Engine.Config(), d, positions)
	if err != nil {
		if !p.failing && p.OnEvent != nil {
			p.OnEvent("flush_failed", "P1", err.Error())
		}
		p.failing = true
		return fmt.Errorf("flush: %w", err)
	}
	p.failing = false
	p.delta = engine.NewDelta()
	for _, name := range names {
		st := p.sources[name]
		st.dirty = false
		ct, hb := st.commitTime, st.heartbeat
		st.pub.applied.Store(uint64(st.pending))
		st.pub.commit.Store(&ct)
		st.pub.hb.Store(&hb)
	}
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
