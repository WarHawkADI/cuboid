package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"cuboid/internal/backfill"
	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/store"
)

// rebuildFromMain rebuilds every number from scratch (bonus B-2): a new slot
// with an exported snapshot, a canary-guarded backfill into shadow tables,
// then one transaction swaps the shadows in and moves the stream to the new
// slot. Queries keep answering from the live tables the whole time, labelled
// with their honest freshness.
func (c *Core) rebuildFromMain(ctx context.Context, reason string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.setState("rebuilding from main")
	c.Rebuild.Start("main", "all stores", reason)
	c.Rec.Annotate("Rebuild from scratch started")
	cfg := c.Config()
	var oldSlot string
	if err := c.Store.W.QueryRow(ctx, "SELECT coalesce(slot, '') FROM cdc_state").Scan(&oldSlot); err != nil {
		return c.Rebuild.Fail(err)
	}
	newSlot := "cuboid_a"
	if oldSlot == "cuboid_a" {
		newSlot = "cuboid_b"
	}
	if err := c.Emitter.DropSlot(ctx, newSlot); err != nil {
		return c.Rebuild.Fail(err)
	}
	if err := c.Store.DropShadows(ctx, cfg); err != nil {
		return c.Rebuild.Fail(err)
	}
	var ddl []string
	for _, name := range cfg.Sources.Keys() {
		ddl = append(ddl, store.MirrorDDL(c.Specs[name], cfg.Tenancy.TenantColumn, "_shadow")...)
	}
	for _, src := range cfg.FactSources() {
		ddl = append(ddl, store.CellDDL(src, "_shadow")...)
	}
	for _, s := range ddl {
		if _, err := c.Store.W.Exec(ctx, s); err != nil {
			return c.Rebuild.Fail(fmt.Errorf("%s: %w", s, err))
		}
	}
	conn, err := cdc.ConnectReplication(ctx, c.Opt.MainCDCDSN)
	if err != nil {
		return c.Rebuild.Fail(err)
	}
	s := &cdc.Stream{Conn: conn, Slot: newSlot, Publication: c.Opt.Publication, Log: c.Log}
	consistent, snapshot, err := s.CreateSlot(ctx, false)
	if err != nil {
		conn.Close(context.Background())
		return c.Rebuild.Fail(err)
	}
	stats, err := backfill.Run(ctx, backfill.Options{
		MainDSN: c.Opt.MainCDCDSN, Snapshot: snapshot, Store: c.Store, Cfg: cfg, Specs: c.Specs,
		Names: store.Shadow, Loc: c.Loc, Now: time.Now, Workers: 4, Throttle: c.canaryThrottle, Log: c.Log,
		Progress: c.Rebuild.Update,
	})
	conn.Close(context.Background()) // the exported snapshot is no longer needed
	if err != nil {
		c.Emitter.DropSlot(context.Background(), newSlot)
		return c.Rebuild.Fail(err)
	}
	tx, err := c.Store.W.Begin(ctx)
	if err != nil {
		return c.Rebuild.Fail(err)
	}
	defer tx.Rollback(context.Background())
	if err := store.SwapIn(ctx, tx, liveTables(cfg), "_shadow"); err != nil {
		return c.Rebuild.Fail(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE cdc_state SET slot = $1, applied_lsn = $2::pg_lsn, backfilled_at = now()`, newSlot, consistent.String()); err != nil {
		return c.Rebuild.Fail(err)
	}
	names := cfg.Rollups.Keys()
	if err := store.SetReady(ctx, tx, names, true); err != nil {
		return c.Rebuild.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Rebuild.Fail(err)
	}
	for _, n := range names {
		c.ready.Store(n, true)
	}
	if oldSlot != "" && oldSlot != newSlot {
		if err := c.Emitter.DropSlot(ctx, oldSlot); err != nil {
			c.Log.Warn("drop previous slot", "slot", oldSlot, "err", err)
		}
	}
	c.engine.ForgetDimensions()
	if err := c.Dict.Load(ctx, c.Store.W); err != nil {
		return c.Rebuild.Fail(err)
	}
	if err := c.loadMembership(ctx); err != nil {
		return c.Rebuild.Fail(err)
	}
	if c.Opt.DemoMode {
		c.EnsureDemoKeys(ctx)
	}
	c.Hist.Invalidate(c.Members.AllTenants())
	go c.Svc.Stats.Refresh(context.Background(), c.Store, cfg)
	msg := fmt.Sprintf("Rebuilt from scratch: %d rows copied in %s, rollups built in %s", stats.Rows, stats.CopyTime.Round(time.Second), stats.BuildTime.Round(time.Second))
	c.Rebuild.Finish(msg)
	c.Rec.Annotate("Rebuilt from scratch")
	c.Log.Info(msg)
	return nil
}

func liveTables(cfg *config.Config) []string {
	var out []string
	for _, name := range cfg.Sources.Keys() {
		out = append(out, config.MirrorTable(name))
	}
	for _, src := range cfg.FactSources() {
		out = append(out, config.CellTable(src.Name))
	}
	return out
}

// RebuildFromMain stops the stream and rebuilds from scratch.
func (c *Core) RebuildFromMain(reason string) {
	c.requestMainRebuild(reason)
	if stop := c.streamStop.Load(); stop != nil {
		(*stop)()
	}
}

// canaryThrottle blocks while the store app's p95 is more than 5% above its
// baseline, backing off exponentially, so a backfill never slows the app.
func (c *Core) canaryThrottle(ctx context.Context) {
	wait := 100 * time.Millisecond
	for i := 0; i < 8; i++ {
		if !c.Rec.AppUnderPressure(1.05) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// ErrBusy means another rebuild or reload is running.
var ErrBusy = errors.New("another rebuild or config reload is running")

// RebuildFromMirror recomputes rollups from the mirror while queries keep
// answering and the stream keeps flushing (bonus B-2, online, zero load on
// the main database). tenants nil means every store; rollups nil means all.
// beforeResume runs on the pipeline goroutine at the snapshot boundary (a
// config reload switches the engine there).
func (c *Core) RebuildFromMirror(ctx context.Context, tenants []int32, rollups []*config.Rollup, beforeResume func(), label string) error {
	if !c.opMu.TryLock() {
		return ErrBusy
	}
	defer c.opMu.Unlock()
	p := c.Pipeline()
	if p == nil {
		return errors.New("the change stream is not running")
	}
	scope := "all stores"
	if tenants != nil {
		scope = fmt.Sprintf("stores %v", tenants)
	}
	c.Rebuild.Start("mirror", scope, label)
	c.Rec.Annotate("Rebuild from the mirror started")

	// 1. Pause at a flush boundary L and take a snapshot of AGG there.
	if _, err := p.Pause(ctx, beforeResume); err != nil {
		return c.Rebuild.Fail(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			p.Resume(context.Background())
		}
	}
	defer resume()
	cfg := c.Config()
	if rollups == nil {
		rollups = cfg.Rollups.Values()
	}
	inScope := map[int16]bool{}
	sources := map[string]*config.Source{}
	for _, r := range rollups {
		inScope[r.ID] = true
		sources[r.Source] = cfg.Sources.Get(r.Source)
	}
	for _, src := range sources {
		stmts := append([]string{"DROP TABLE IF EXISTS " + store.QuoteIdent(config.CellTable(src.Name)+"_shadow")}, store.CellDDL(src, "_shadow")...)
		for _, s := range stmts {
			if _, err := c.Store.W.Exec(ctx, s); err != nil {
				return c.Rebuild.Fail(err)
			}
		}
	}
	for _, s := range store.SeedDictionary(cfg, store.Names{}) {
		if _, err := c.Store.W.Exec(ctx, s); err != nil {
			return c.Rebuild.Fail(err)
		}
	}
	snap, err := c.Store.W.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return c.Rebuild.Fail(err)
	}
	defer snap.Rollback(context.Background())
	if _, err := snap.Exec(ctx, "SELECT 1"); err != nil { // the snapshot is taken here, at L
		return c.Rebuild.Fail(err)
	}
	// 2. Log every flush after L, and let the stream continue.
	p.StartDeltaLog()
	resume()
	resumed = false

	// 3. Recompute the scope into shadow cells from the snapshot.
	names := store.Names{CellSuffix: "_shadow"}
	for i, r := range rollups {
		for _, st := range store.BuildRollup(cfg, r, c.Loc, time.Now(), names, tenants) {
			if _, err := snap.Exec(ctx, st.SQL, st.Args...); err != nil {
				p.TakeDeltaLog(false)
				return c.Rebuild.Fail(fmt.Errorf("build %s: %w", r.Name, err))
			}
		}
		c.Rebuild.Update("build", int64(i+1), int64(len(rollups)))
	}
	if err := snap.Commit(ctx); err != nil {
		p.TakeDeltaLog(false)
		return c.Rebuild.Fail(err)
	}
	shadow := *c.flusher
	shadow.Names = names
	filter := func(d map[engine.CellKey][]int64) map[engine.CellKey][]int64 {
		out := map[engine.CellKey][]int64{}
		for k, v := range d {
			if inScope[k.Rollup] && (tenants == nil || containsInt32(tenants, k.Tenant)) {
				out[k] = v
			}
		}
		return out
	}
	apply := func(d map[engine.CellKey][]int64) error {
		d = filter(d)
		if len(d) == 0 {
			return nil
		}
		tx, err := c.Store.W.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if err := shadow.WriteCells(ctx, tx, cfg, d); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// 4. Catch the shadow up with the logged deltas while the stream runs.
	for i := 0; i < 5; i++ {
		d := p.TakeDeltaLog(true)
		if err := apply(d); err != nil {
			p.TakeDeltaLog(false)
			return c.Rebuild.Fail(err)
		}
		if len(d) < 2000 {
			break
		}
	}
	// 5. Pause briefly: apply the last deltas and swap in one transaction.
	if _, err := p.Pause(ctx, nil); err != nil {
		p.TakeDeltaLog(false)
		return c.Rebuild.Fail(err)
	}
	if err := apply(p.TakeDeltaLog(false)); err != nil {
		return c.Rebuild.Fail(err)
	}
	tx, err := c.Store.W.Begin(ctx)
	if err != nil {
		return c.Rebuild.Fail(err)
	}
	defer tx.Rollback(context.Background())
	whole := tenants == nil && len(rollups) == cfg.Rollups.Len()
	for _, src := range sources {
		live := config.CellTable(src.Name)
		if whole && len(sources) == len(cfg.FactSources()) {
			if err := store.SwapIn(ctx, tx, []string{live}, "_shadow"); err != nil {
				return c.Rebuild.Fail(err)
			}
			continue
		}
		var ids []int16
		for id := range inScope {
			if cfg.RollupByID(id).Source == src.Name {
				ids = append(ids, id)
			}
		}
		del := fmt.Sprintf("DELETE FROM %s WHERE rollup = ANY($1)", store.QuoteIdent(live))
		args := []any{ids}
		if tenants != nil {
			del += " AND tenant_id = ANY($2)"
			args = append(args, tenants)
		}
		if _, err := tx.Exec(ctx, del, args...); err != nil {
			return c.Rebuild.Fail(err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", store.QuoteIdent(live), store.QuoteIdent(live+"_shadow"))); err != nil {
			return c.Rebuild.Fail(err)
		}
		if _, err := tx.Exec(ctx, "DROP TABLE "+store.QuoteIdent(live+"_shadow")); err != nil {
			return c.Rebuild.Fail(err)
		}
	}
	var rnames []string
	for _, r := range rollups {
		rnames = append(rnames, r.Name)
	}
	if err := store.SetReady(ctx, tx, rnames, true); err != nil {
		return c.Rebuild.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Rebuild.Fail(err)
	}
	for _, n := range rnames {
		c.ready.Store(n, true)
	}
	if tenants == nil {
		c.Hist.Invalidate(c.Members.AllTenants())
	} else {
		c.Hist.Invalidate(tenants)
	}
	resume()
	msg := fmt.Sprintf("Rebuilt %s for %s from the mirror", strings.Join(rnames, ", "), scope)
	c.Rebuild.Finish(msg)
	c.Rec.Annotate("Rebuilt from the mirror")
	return nil
}

func containsInt32(xs []int32, x int32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
