package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"

	"cuboid/internal/audit"
	"cuboid/internal/backfill"
	"cuboid/internal/cdc"
	"cuboid/internal/store"
	"cuboid/internal/verify"
)

// A source is one database Cuboid streams from: the platform's, which holds
// the registry of stores, or one store's own (ADR-012). Each has its own
// replication slot, stream and applied position. A source is loaded, and
// reloaded after a TRUNCATE, a lost slot or a failed check, on its own: every
// other source keeps streaming meanwhile.
type source struct {
	name    string // the database
	tenant  int32  // the store whose database it is; 0 for the platform's
	dsn     string // cuboid_cdc on that database
	emitter *cdc.Emitter
	cancel  context.CancelFunc
	done    chan struct{}
	reload  chan string // a requested reload's reason
	stop    atomic.Pointer[context.CancelFunc]
	state   atomic.Value // string
	loads   atomic.Int64
	lastErr atomic.Value // string
}

func (s *source) State() string {
	v, _ := s.state.Load().(string)
	return v
}

func (s *source) setState(v string) { s.state.Store(v) }

// requestReload asks the source to reload from a new snapshot of its
// database, ending its stream session if one runs.
func (s *source) requestReload(reason string) {
	select {
	case s.reload <- reason:
	default: // one is already queued
	}
	if stop := s.stop.Load(); stop != nil {
		(*stop)()
	}
}

func (s *source) takeReload() string {
	select {
	case r := <-s.reload:
		return r
	default:
		return ""
	}
}

var errStopForReload = errors.New("stopped for a reload from the source database")

// SourceStatus is a source as the monitoring page shows it.
type SourceStatus struct {
	Name       string `json:"name"`
	Tenant     int32  `json:"tenant_id,omitempty"`
	State      string `json:"state"`
	AppliedLSN string `json:"applied_lsn"`
	LagMS      int64  `json:"lag_ms"`
	Loads      int64  `json:"loads"`
	Error      string `json:"error,omitempty"`
}

// Sources lists every source and its state, the platform's first.
func (c *Core) Sources() []SourceStatus {
	c.srcMu.Lock()
	list := make([]*source, 0, len(c.sources))
	for _, s := range c.sources {
		list = append(list, s)
	}
	c.srcMu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if (list[i].tenant == 0) != (list[j].tenant == 0) {
			return list[i].tenant == 0
		}
		return list[i].tenant < list[j].tenant
	})
	out := make([]SourceStatus, len(list))
	for i, s := range list {
		st := SourceStatus{Name: s.name, Tenant: s.tenant, State: s.State(), AppliedLSN: c.pipe.Load().Applied(s.name).String(), LagMS: -1, Loads: s.loads.Load()}
		commit, hb := c.pipe.Load().AppliedTimes(s.name)
		if as := latest(commit, hb); !as.IsZero() {
			st.LagMS = time.Since(as).Milliseconds()
		}
		if e, ok := s.lastErr.Load().(string); ok {
			st.Error = e
		}
		out[i] = st
	}
	return out
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// ReloadStores reloads the given stores from their own databases, or every
// source when tenants is nil (bonus B-2, "rebuild from scratch"). Each store
// reloads in its own transaction while the others keep streaming.
func (c *Core) ReloadStores(tenants []int32, reason string) error {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	if len(c.sources) == 0 {
		return errors.New("no source is running yet")
	}
	var matched []*source
	for _, s := range c.sources {
		if tenants == nil || containsInt32(tenants, s.tenant) || (s.tenant == 0 && !c.Config().PerStore()) {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no source holds stores %v", tenants)
	}
	// Shown as running from the request on, not from the first load's start.
	if !c.Rebuild.Snapshot().Running {
		c.Rebuild.Start("main", fmt.Sprintf("%d sources", len(matched)), reason)
	}
	for _, s := range matched {
		s.setState("queued for a load")
		s.requestReload(reason)
	}
	c.setState(fmt.Sprintf("loading %d of %d sources", len(matched), len(c.sources)))
	return nil
}

// supervise keeps one goroutine per source database: it starts a source as
// soon as its database exists, and stops them all while a bulk load runs.
func (c *Core) supervise(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		c.reconcile(ctx)
		select {
		case <-ctx.Done():
			c.stopSources()
			return
		case <-t.C:
		}
	}
}

func (c *Core) reconcile(ctx context.Context) {
	if c.Emitter.Loading(ctx) {
		if !c.dataLoad.Swap(true) {
			c.Log.Info("a bulk data load is running; every source stops until it ends")
			c.stopSources()
		}
		c.setState("waiting for the data load")
		return
	}
	c.dataLoad.Store(false)
	want, err := c.wantedSources(ctx)
	if err != nil {
		c.Log.Warn("list the source databases", "err", err)
		return
	}
	c.srcMu.Lock()
	for name, s := range c.sources {
		if want[name] == nil {
			delete(c.sources, name)
			go c.removeSource(ctx, s)
		}
	}
	for name, s := range want {
		if c.sources[name] != nil {
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		s.cancel, s.done, s.reload = cancel, make(chan struct{}), make(chan string, 1)
		s.emitter = cdc.NewEmitter(s.dsn)
		s.setState("starting")
		c.sources[name] = s
		go c.runSource(sctx, s)
	}
	c.srcMu.Unlock()
	c.updateState(ctx)
}

// wantedSources is the platform's database, and with per-store databases
// every store in the registry whose database exists.
func (c *Core) wantedSources(ctx context.Context) (map[string]*source, error) {
	want := map[string]*source{c.platformDB: {name: c.platformDB, dsn: c.Opt.MainCDCDSN}}
	cfg := c.Config()
	if !cfg.PerStore() {
		return want, nil
	}
	tc := cfg.Tenancy.Company
	rows, err := c.platform.Query(ctx, fmt.Sprintf("SELECT %s FROM %s ORDER BY 1", pgx.Identifier{tc.Key}.Sanitize(), pgx.Identifier{tc.Table}.Sanitize()))
	if err != nil {
		return nil, err
	}
	var tenants []int32
	for rows.Next() {
		var t int32
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	dbs, err := c.Emitter.Databases(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tenants {
		db := cfg.StoreDatabase(t)
		if !dbs[db] {
			continue // registered, but its database is not created yet
		}
		dsn, err := cdc.WithDatabase(c.Opt.MainCDCDSN, db)
		if err != nil {
			return nil, err
		}
		want[db] = &source{name: db, tenant: t, dsn: dsn}
	}
	return want, nil
}

// removeSource ends a store that left the registry or whose database is
// gone: its stream stops, its slots are dropped, and its rows leave the
// aggregate store in one transaction, so it is never answered from stale data.
func (c *Core) removeSource(ctx context.Context, s *source) {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
	}
	for _, slot := range []string{"cuboid_" + s.name + "_a", "cuboid_" + s.name + "_b"} {
		if err := c.Emitter.DropSlot(ctx, slot); err != nil {
			c.Log.Warn("drop the slot of a removed store", "slot", slot, "err", err)
		}
	}
	if err := c.purge(ctx, s); err != nil {
		c.Log.Error("purge a removed store", "source", s.name, "err", err)
		return
	}
	if s.tenant != 0 {
		c.Hist.Invalidate([]int32{s.tenant})
	}
	c.Audit.Event(audit.Event{Kind: "source_removed", Severity: "P3", Tenant: s.tenant,
		Detail: fmt.Sprintf("%s left the registry or was dropped: its stream stopped and its rows left the aggregate store", s.name)})
	c.Rec.Annotate("Removed " + s.name)
	c.Log.Info("source removed", "source", s.name)
}

func (c *Core) purge(ctx context.Context, s *source) error {
	tx, err := c.Store.W.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	cfg := c.Config()
	if s.tenant != 0 {
		tcol := pgx.Identifier{cfg.Tenancy.TenantColumn}.Sanitize()
		for _, src := range cfg.SourcesOn(false) {
			if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = $1", store.Names{}.Mirror(src.Name), tcol), s.tenant); err != nil {
				return err
			}
		}
		for _, src := range cfg.FactSources() {
			if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = $1", store.Names{}.Cells(src.Name)), s.tenant); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM cdc_sources WHERE source = $1", s.name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// stopSources ends every source and waits for them to stop.
func (c *Core) stopSources() {
	c.srcMu.Lock()
	list := make([]*source, 0, len(c.sources))
	for _, s := range c.sources {
		list = append(list, s)
	}
	c.sources = map[string]*source{}
	c.srcMu.Unlock()
	for _, s := range list {
		s.cancel()
	}
	for _, s := range list {
		select {
		case <-s.done:
		case <-time.After(30 * time.Second):
			c.Log.Warn("a source did not stop in time", "source", s.name)
		}
	}
}

// updateState sums up the sources: streaming once every one streams.
func (c *Core) updateState(ctx context.Context) {
	c.srcMu.Lock()
	total, streaming, loading := len(c.sources), 0, 0
	for _, s := range c.sources {
		switch st := s.State(); {
		case st == "streaming":
			streaming++
		case strings.HasPrefix(st, "loading") || st == "queued for a load":
			loading++
		}
	}
	c.srcMu.Unlock()
	switch {
	case total > 0 && streaming == total:
		if c.State() != "streaming" {
			c.setState("streaming")
		}
		if c.analyzeDue.Swap(false) {
			if err := backfill.Analyze(ctx, c.Store, c.Config()); err != nil {
				c.Log.Warn("analyze after loads", "err", err)
			}
		}
	case loading > 0:
		c.setState(fmt.Sprintf("loading %d of %d sources", loading, total))
	case total > 0:
		c.setState(fmt.Sprintf("starting: %d of %d sources streaming", streaming, total))
	}
}

// runSource keeps one source streaming, loading it from a snapshot first
// when it has never been loaded, its slot is gone or a reload was asked for.
func (c *Core) runSource(ctx context.Context, s *source) {
	defer close(s.done)
	defer s.emitter.Close()
	go s.emitter.Heartbeats(ctx, time.Second, func(err error) { c.Log.Debug("heartbeat", "source", s.name, "err", err) })
	backoff := time.Second
	for ctx.Err() == nil {
		reason := s.takeReload()
		row, err := c.sourceRow(ctx, s.name)
		if err != nil {
			c.sleep(ctx, backoff)
			continue
		}
		if reason == "" {
			switch {
			case !row.found || row.backfilled == nil:
				reason = "first load"
			case row.slot == "":
				reason = "it has no replication slot"
			default:
				h, err := c.Emitter.Slots(ctx, row.slot)
				if err != nil {
					c.sleep(ctx, backoff)
					continue
				}
				if !h[row.slot].Exists {
					reason = "its replication slot is missing, so its stream cannot resume exactly"
				}
			}
		}
		if reason != "" {
			if err := c.loadSource(ctx, s, row.slot, reason); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.lastErr.Store(err.Error())
				s.setState("load failed, retrying")
				c.Alarms.Raise("load_failed_"+s.name, fmt.Sprintf("Loading %s failed: %v. Retrying.", s.name, err))
				c.Log.Error("load a source", "source", s.name, "err", err)
				s.requestReload(reason)
				c.sleep(ctx, backoff)
				backoff = min(2*backoff, 30*time.Second)
				continue
			}
			c.Alarms.Clear("load_failed_" + s.name)
			backoff = time.Second
			continue
		}
		if err := c.pipe.Load().Source(ctx, s.name, s.tenant, row.applied, row.commit, row.heartbeat); err != nil {
			continue
		}
		err = c.streamSource(ctx, s, row.slot, row.applied)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, cdc.ErrTruncated):
			s.requestReload("a table was truncated in " + s.name)
		case errors.Is(err, errStopForReload):
		case err != nil:
			s.lastErr.Store(err.Error())
			s.setState("reconnecting")
			c.Alarms.Raise("stream_error_"+s.name, fmt.Sprintf("The stream of %s stopped: %v. Reconnecting.", s.name, err))
			c.Log.Error("stream", "source", s.name, "err", err)
			c.sleep(ctx, backoff)
			backoff = min(2*backoff, 10*time.Second)
			continue
		}
		backoff = time.Second
	}
}

func (c *Core) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

type sourceRow struct {
	found      bool
	slot       string
	applied    pglogrepl.LSN
	backfilled *time.Time
	commit     time.Time
	heartbeat  time.Time
}

func (c *Core) sourceRow(ctx context.Context, name string) (sourceRow, error) {
	var r sourceRow
	var applied string
	var commit, hb *time.Time
	err := c.Store.W.QueryRow(ctx, `SELECT coalesce(slot, ''), applied_lsn::text, backfilled_at, applied_commit_time, heartbeat_time
		FROM cdc_sources WHERE source = $1`, name).Scan(&r.slot, &applied, &r.backfilled, &commit, &hb)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.found = true
	if r.applied, err = pglogrepl.ParseLSN(applied); err != nil {
		return r, err
	}
	if commit != nil {
		r.commit = *commit
	}
	if hb != nil {
		r.heartbeat = *hb
	}
	return r, nil
}

// streamSource runs one replication session of a source.
func (c *Core) streamSource(ctx context.Context, s *source, slot string, start pglogrepl.LSN) error {
	sctx, cancel := context.WithCancelCause(ctx)
	stop := context.CancelFunc(func() { cancel(errStopForReload) })
	s.stop.Store(&stop)
	defer func() { s.stop.Store(nil); cancel(nil) }()
	conn, err := cdc.ConnectReplication(sctx, s.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	s.setState("streaming")
	c.Alarms.Clear("stream_error_" + s.name)
	c.updateState(ctx)
	st := &cdc.Stream{Conn: conn, Slot: slot, Publication: c.Opt.Publication, Log: c.Log.With("source", s.name)}
	err = st.Run(sctx, start, c.pipe.Load().Handler(s.name))
	if cause := context.Cause(sctx); errors.Is(cause, errStopForReload) {
		return errStopForReload
	}
	return err
}

// slotName alternates a source's slot between two names, so a reload creates
// its new slot before it drops the old one.
func slotName(db, old string) string {
	if strings.HasSuffix(old, "_a") {
		return "cuboid_" + db + "_b"
	}
	return "cuboid_" + db + "_a"
}

// loadSource replaces everything the aggregate store holds from one source
// with a consistent snapshot of its database, in one transaction, and moves
// its stream to the new slot. Stores load in parallel, a few at a time; none
// runs while a rebuild from the mirror holds the aggregate store's cells.
func (c *Core) loadSource(ctx context.Context, s *source, oldSlot, reason string) error {
	s.setState("queued for a load")
	c.updateState(ctx)
	c.opMu.RLock()
	defer c.opMu.RUnlock()
	select {
	case c.loadSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.loadSlots }()
	s.setState("loading: " + reason)
	c.updateState(ctx)
	c.loadBatchStart(s, reason)
	began := time.Now()
	p := c.pipe.Load()
	// Whatever this source's stream delivered goes in first: the load
	// replaces it, so none of it can land afterwards and count twice.
	if err := p.Flush(ctx); err != nil {
		return c.loadBatchEnd(s, 0, err)
	}
	cfg := c.Config()
	slot := slotName(s.name, oldSlot)
	if err := s.emitter.DropSlot(ctx, slot); err != nil {
		return c.loadBatchEnd(s, 0, err)
	}
	consistent, stats, err := backfill.Load(ctx,
		backfill.Source{Name: s.name, DSN: s.dsn, Tenant: s.tenant, Tables: cfg.SourcesOn(s.tenant == 0), Slot: slot},
		backfill.Options{Store: c.Store, Cfg: cfg, Specs: c.Specs, Loc: c.Loc, Now: time.Now, Throttle: c.canaryThrottle, Log: c.Log})
	if err != nil {
		return c.loadBatchEnd(s, 0, err)
	}
	if oldSlot != "" && oldSlot != slot {
		if err := s.emitter.DropSlot(ctx, oldSlot); err != nil {
			c.Log.Warn("drop the previous slot", "slot", oldSlot, "err", err)
		}
	}
	now := time.Now()
	if err := p.Source(ctx, s.name, s.tenant, consistent, now, now); err != nil {
		return c.loadBatchEnd(s, stats.Rows, err)
	}
	if err := c.Dict.Load(ctx, c.Store.W); err != nil {
		return c.loadBatchEnd(s, stats.Rows, err)
	}
	if s.tenant != 0 {
		p.ForgetTenant(ctx, s.tenant)
		c.Hist.Invalidate([]int32{s.tenant})
	} else {
		p.Do(ctx, func() error { p.Engine.ForgetDimensions(); return nil })
		if err := c.loadMembership(ctx); err != nil {
			c.Log.Warn("membership", "err", err)
		}
		if c.Opt.DemoMode {
			c.EnsureDemoKeys(ctx)
		}
		c.Hist.Invalidate(c.Members.AllTenants())
	}
	s.loads.Add(1)
	s.lastErr.Store("")
	c.analyzeDue.Store(true)
	c.Log.Info("source loaded", "source", s.name, "reason", reason, "rows", stats.Rows, "copy", stats.CopyTime.Round(time.Millisecond),
		"build", stats.BuildTime.Round(time.Millisecond), "took", time.Since(began).Round(time.Millisecond))
	if reason != "first load" {
		c.Audit.Event(audit.Event{Kind: "source_reloaded", Severity: "P3", Tenant: s.tenant,
			Detail: fmt.Sprintf("%s was reloaded from its database (%s): %d rows; every other source kept streaming", s.name, reason, stats.Rows)})
		c.Rec.Annotate(fmt.Sprintf("Reloaded %s from its database", s.name))
	}
	return c.loadBatchEnd(s, stats.Rows, nil)
}

// loadBatch follows the loads in flight for the monitoring page's rebuild
// panel: a batch starts with the first load and ends when none is left.
type loadBatch struct {
	mu      sync.Mutex
	active  map[string]bool
	done    int64
	failed  int64
	rows    int64
	started time.Time
}

func (c *Core) loadBatchStart(s *source, reason string) {
	b := &c.loads
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.active) == 0 {
		b.active, b.done, b.failed, b.rows, b.started = map[string]bool{}, 0, 0, 0, time.Now()
		if !c.Rebuild.Snapshot().Running {
			c.Rebuild.Start("main", "sources", reason)
		}
	}
	b.active[s.name] = true
	c.Rebuild.Update("load", b.done, b.done+int64(len(b.active))+c.queuedLoads())
}

func (c *Core) loadBatchEnd(s *source, rows int64, err error) error {
	b := &c.loads
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.active, s.name)
	if err != nil {
		b.failed++
	} else {
		b.done++
		b.rows += rows
	}
	queued := c.queuedLoads()
	c.Rebuild.Update("load", b.done, b.done+int64(len(b.active))+queued)
	if len(b.active) == 0 && queued == 0 {
		msg := fmt.Sprintf("Loaded %d source databases from snapshots, each in its own transaction: %d rows in %s", b.done, b.rows, time.Since(b.started).Round(time.Second))
		if b.failed > 0 {
			msg += fmt.Sprintf("; %d loads failed and are retried", b.failed)
		}
		c.Rebuild.Finish(msg)
		c.Log.Info(msg)
	}
	return err
}

func (c *Core) queuedLoads() int64 {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	var n int64
	for _, s := range c.sources {
		if s.State() == "queued for a load" {
			n++
		}
	}
	return n
}

// verifySources returns the source databases holding the given stores, for a
// fenced check: each store's own, or the one database every store shares.
func (c *Core) verifySources(tenants []int32) ([]verify.Source, error) {
	cfg := c.Config()
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	if !cfg.PerStore() {
		s := c.sources[c.platformDB]
		if s == nil || s.State() != "streaming" {
			return nil, errors.New("the change stream is not running right now; try again in a moment")
		}
		return []verify.Source{{Name: s.name, Tenants: tenants, Emitter: s.emitter, VerifyDSN: c.Opt.MainVerifyDSN}}, nil
	}
	var out []verify.Source
	for _, t := range tenants {
		var s *source
		for _, x := range c.sources {
			if x.tenant == t {
				s = x
			}
		}
		if s == nil || s.State() != "streaming" {
			state := "not running"
			if s != nil {
				state = s.State()
			}
			return nil, fmt.Errorf("store %d's database is not streaming right now (%s); try again in a moment", t, state)
		}
		dsn, err := cdc.WithDatabase(c.Opt.MainVerifyDSN, s.name)
		if err != nil {
			return nil, err
		}
		out = append(out, verify.Source{Name: s.name, Tenants: []int32{t}, Emitter: s.emitter, VerifyDSN: dsn})
	}
	return out, nil
}
