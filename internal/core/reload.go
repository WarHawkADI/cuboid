package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/fsnotify/fsnotify"

	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/store"
)

// ReloadReport says what a config reload did.
type ReloadReport struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Added   []string `json:"added_rollups,omitempty"`
	Removed []string `json:"removed_rollups,omitempty"`
	Rebuilt []string `json:"rebuilt_rollups,omitempty"`
	TookMS  int64    `json:"took_ms"`
}

// Reload applies config/metrics.yaml without a restart (bonus B-4). New
// rollups and measures are built from the mirror, so the main database is
// not touched, and become queryable once built. An invalid file leaves the
// running configuration in place and names the offending line.
//
// One reload runs at a time. Saving the file reaches here twice, from the file
// watcher and from whoever saved it, so the second finds nothing left to do.
func (c *Core) Reload(ctx context.Context) *ReloadReport {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	start := time.Now()
	rep := &ReloadReport{}
	fail := func(err error) *ReloadReport {
		rep.Error = err.Error()
		rep.TookMS = time.Since(start).Milliseconds()
		c.Log.Warn("config reload rejected", "err", err)
		return rep
	}
	raw, err := os.ReadFile(c.Opt.MetricsPath)
	if err != nil {
		return fail(err)
	}
	if bytes.Equal(raw, c.appliedConfig) {
		rep.OK = true
		return rep
	}
	old := c.Config()
	next, err := config.Load(c.Opt.MetricsPath)
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(old.Sources.Keys(), next.Sources.Keys()) {
		return fail(fmt.Errorf("adding or removing source tables changes the publication and the mirror; restart Cuboid for that"))
	}
	if !reflect.DeepEqual(old.Tenancy, next.Tenancy) {
		return fail(fmt.Errorf("tenancy decides which databases are streamed; restart Cuboid to change it"))
	}
	plain, err := cdc.ConnectPlain(ctx, c.Opt.MainCDCDSN, "cuboid-reload")
	if err != nil {
		return fail(err)
	}
	all, _, checks, err := catalog(ctx, plain, c.Opt.MainCDCDSN, next)
	plain.Close(ctx)
	if err != nil {
		return fail(err)
	}
	if err := next.ValidateColumns(all); err != nil {
		return fail(err)
	}
	if err := next.ValidateNonnegative(checks); err != nil {
		return fail(err)
	}
	var build []*config.Rollup
	changedMeasures := map[string]bool{}
	for _, src := range next.FactSources() {
		if !reflect.DeepEqual(old.Sources.Get(src.Name).Measures.Keys(), src.Measures.Keys()) {
			changedMeasures[src.Name] = true
		}
	}
	for _, r := range next.Rollups.Values() {
		prev := old.Rollups.Get(r.Name)
		switch {
		case prev == nil:
			rep.Added = append(rep.Added, r.Name)
			build = append(build, r)
		case !reflect.DeepEqual(prev.Dims, r.Dims) || !reflect.DeepEqual(prev.GrainSpec, r.GrainSpec) || prev.Source != r.Source:
			return fail(fmt.Errorf("line %d: rollup %s already exists with a different definition; add the new shape under a new name, then remove the old one", r.Line, r.Name))
		case changedMeasures[r.Source]:
			build = append(build, r)
		}
	}
	for _, name := range old.Rollups.Keys() {
		if next.Rollups.Get(name) == nil {
			rep.Removed = append(rep.Removed, name)
		}
	}
	// ALTER TABLE takes an exclusive lock even when it changes nothing, and under
	// load that deadlocks with live reads. A new rollup needs no new column.
	if !reflect.DeepEqual(old.Sources, next.Sources) {
		if err := c.Store.EnsureTables(ctx, next, c.Specs); err != nil {
			return fail(err)
		}
	}
	if _, err := c.Store.RegisterRollups(ctx, next); err != nil {
		return fail(err)
	}
	for _, r := range rep.Added {
		c.ready.Store(r, false)
	}
	switchEngine := func() {
		c.engine.SetConfig(next)
		c.cfg.Store(next)
	}
	if len(build) == 0 {
		if p := c.Pipeline(); p != nil {
			if err := p.Pause(ctx, switchEngine); err != nil {
				return fail(err)
			}
			if err := p.Resume(ctx); err != nil {
				return fail(err)
			}
		} else {
			switchEngine()
		}
	} else {
		if err := c.RebuildFromMirror(ctx, nil, build, switchEngine, "config reload"); err != nil {
			return fail(err)
		}
		for _, r := range build {
			rep.Rebuilt = append(rep.Rebuilt, r.Name)
		}
	}
	c.Svc.SetActive(c.active(next))
	for _, name := range rep.Removed {
		if r := old.Rollups.Get(name); r != nil {
			c.Store.W.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE rollup = $1", store.Names{}.Cells(r.Source)), r.ID)
			c.Store.W.Exec(ctx, "DELETE FROM rollup_registry WHERE name = $1", name)
		}
	}
	c.appliedConfig = raw
	rep.OK = true
	rep.TookMS = time.Since(start).Milliseconds()
	c.Rec.Annotate(fmt.Sprintf("Config reloaded: %d rollups added", len(rep.Added)))
	c.Log.Info("config reloaded", "added", rep.Added, "removed", rep.Removed, "rebuilt", rep.Rebuilt, "took", time.Since(start))
	return rep
}

// WatchConfig reloads when the metrics file changes.
func (c *Core) WatchConfig(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		c.Log.Warn("config watcher", "err", err)
		return
	}
	defer w.Close()
	dir := filepath.Dir(c.Opt.MetricsPath)
	if err := w.Add(dir); err != nil {
		c.Log.Warn("config watcher", "err", err)
		return
	}
	var timer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Events:
			if filepath.Clean(ev.Name) == filepath.Clean(c.Opt.MetricsPath) && ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				timer = time.After(500 * time.Millisecond) // editors write in bursts
			}
		case <-timer:
			c.lastReload.Store(c.Reload(ctx))
		case err := <-w.Errors:
			c.Log.Warn("config watcher", "err", err)
		}
	}
}
