// Package core assembles a running Cuboid: the change stream, the engine and
// flusher, the query API, rebuilds, live config reload, verification and the
// monitoring state. cmd/cuboid is a thin wrapper around it.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"cuboid/internal/audit"
	"cuboid/internal/auth"
	"cuboid/internal/cache"
	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/flush"
	"cuboid/internal/limiter"
	"cuboid/internal/monitor"
	"cuboid/internal/pipeline"
	"cuboid/internal/planner"
	"cuboid/internal/query"
	"cuboid/internal/store"
	"cuboid/internal/verify"
)

// Options configure a Cuboid process. Every value comes from CUBOID_*
// environment variables (cmd/cuboid).
type Options struct {
	MainCDCDSN    string
	MainVerifyDSN string
	AggWriterDSN  string
	AggReaderDSN  string
	MetricsPath   string
	LimitsPath    string
	Pepper        string
	DemoMode      bool
	DemoSeed      string
	FlushInterval time.Duration
	Publication   string
	StoreAppURL   string // storesim, for the canary and the recorder
	LoadgenURL    string // dashboard load generator, for the path switch
	NaiveURL      string // the direct baseline, for its query rate
	ArtifactsDir  string
	WebDir        string
	CORSOrigins   []string // browser origins allowed besides the UI's own
	Log           *slog.Logger
}

// Core is a running Cuboid.
type Core struct {
	Opt     Options
	Log     *slog.Logger
	Loc     *time.Location
	Store   *store.Store
	Specs   map[string]*store.Table
	Dict    *store.Dict
	Emitter *cdc.Emitter
	Main    *pgxpool.Pool // cuboid_verify only
	Svc     *query.Service
	API     *query.API
	Hub     *query.Hub
	Keys    *auth.Keys
	Members *auth.Membership
	Audit   *audit.Log
	quick   quickState
	// reloadMu serialises config reloads; appliedConfig is the file last applied.
	reloadMu      sync.Mutex
	appliedConfig []byte
	Verify        *verify.Verifier
	Limits        *limiter.Limiter
	Hist          *cache.History
	Rec           *monitor.Recorder
	Alarms        *monitor.Alarms
	Rebuild       *monitor.Progress
	Sampler       *verify.Sampler

	cfg     atomic.Pointer[config.Config] // the engine's configuration
	engine  *engine.Engine
	flusher *flush.Flusher
	pipe    atomic.Pointer[pipeline.Pipeline] // one for the process; every source feeds it
	ready   sync.Map                          // rollup name -> bool
	state   atomic.Value

	// opMu: a rebuild from the mirror or a config reload holds it alone;
	// loads of source databases share it, and run in parallel.
	opMu       sync.RWMutex
	platformDB string        // the main DSN's database: the store registry
	platform   *pgxpool.Pool // cuboid_cdc on it, to read the registry
	srcMu      sync.Mutex
	sources    map[string]*source
	loadSlots  chan struct{} // loads that may run at once
	loads      loadBatch
	dataLoad   atomic.Bool // a bulk load runs on the main server
	analyzeDue atomic.Bool
	StartedAt  time.Time

	lastAutoRebuild atomic.Int64
	lastMode        atomic.Pointer[loadState]
	lastReload      atomic.Pointer[ReloadReport]
	lastDrill       atomic.Pointer[DrillReport]
	lastFlood       atomic.Pointer[FloodReport]
	batchMu         sync.Mutex
	batch           BatchStatus
}

// State is what the layer is doing now.
func (c *Core) State() string {
	s, _ := c.state.Load().(string)
	return s
}

func (c *Core) setState(s string) {
	c.state.Store(s)
	c.Log.Info("state", "now", s)
}

// Config returns the engine's active configuration.
func (c *Core) Config() *config.Config { return c.cfg.Load() }

// Pipeline returns the running pipeline, or nil between stream sessions.
func (c *Core) Pipeline() *pipeline.Pipeline { return c.pipe.Load() }

// New connects everything and checks every invariant that can be checked
// before streaming starts.
func New(ctx context.Context, opt Options) (*Core, error) {
	c := &Core{Opt: opt, Log: opt.Log, StartedAt: time.Now(), sources: map[string]*source{}, loadSlots: make(chan struct{}, 4)}
	c.setState("starting")
	// I1: the only main-database roles Cuboid may hold.
	if err := cdc.CheckRole(ctx, opt.MainCDCDSN, "cuboid_cdc"); err != nil {
		return nil, err
	}
	if err := cdc.CheckRole(ctx, opt.MainVerifyDSN, "cuboid_verify"); err != nil {
		return nil, err
	}
	cfg, err := config.Load(opt.MetricsPath)
	if err != nil {
		return nil, err
	}
	lim, err := limiter.LoadConfig(opt.LimitsPath)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, opt.AggWriterDSN, opt.AggReaderDSN, int32(lim.GlobalConcurrency)+8)
	if err != nil {
		return nil, err
	}
	c.Store = st
	if err := st.ApplySchema(ctx); err != nil {
		return nil, err
	}
	plain, err := cdc.ConnectPlain(ctx, opt.MainCDCDSN, "cuboid-setup")
	if err != nil {
		return nil, err
	}
	defer plain.Close(ctx)
	var tz string
	if err := plain.QueryRow(ctx, "SHOW TimeZone").Scan(&tz); err != nil {
		return nil, err
	}
	if cfg.Timezone != "from_main_db" {
		tz = cfg.Timezone
	}
	if c.Loc, err = time.LoadLocation(tz); err != nil {
		return nil, fmt.Errorf("main database time zone %q: %w", tz, err)
	}
	all, specs, checks, err := catalog(ctx, plain, opt.MainCDCDSN, cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateColumns(all); err != nil {
		return nil, err
	}
	if err := cfg.ValidateNonnegative(checks); err != nil {
		return nil, err
	}
	c.platformDB = plain.Config().Database
	pcfg, err := pgxpool.ParseConfig(opt.MainCDCDSN)
	if err != nil {
		return nil, err
	}
	delete(pcfg.ConnConfig.RuntimeParams, "replication")
	pcfg.MaxConns = 2
	pcfg.ConnConfig.RuntimeParams["application_name"] = "cuboid-registry"
	if c.platform, err = pgxpool.NewWithConfig(ctx, pcfg); err != nil {
		return nil, err
	}
	c.Specs = specs
	if err := st.EnsureTables(ctx, cfg, specs); err != nil {
		return nil, err
	}
	ready, err := st.RegisterRollups(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for name, r := range ready {
		c.ready.Store(name, r)
	}
	if err := st.DropShadows(ctx, cfg); err != nil {
		return nil, err
	}
	c.cfg.Store(cfg)
	c.Dict = store.NewDict()
	if err := c.Dict.Load(ctx, st.W); err != nil {
		return nil, err
	}
	c.Keys = auth.NewKeys(opt.Pepper)
	c.Members = auth.NewMembership()
	if err := c.loadMembership(ctx); err != nil {
		return nil, err
	}
	if opt.DemoMode {
		if err := c.EnsureDemoKeys(ctx); err != nil {
			return nil, err
		}
	}
	if err := c.Keys.Load(ctx, st.W); err != nil {
		return nil, err
	}
	c.Emitter = cdc.NewEmitter(opt.MainCDCDSN)
	mcfg, err := pgxpool.ParseConfig(opt.MainVerifyDSN)
	if err != nil {
		return nil, err
	}
	mcfg.MaxConns = 2
	mcfg.ConnConfig.RuntimeParams["application_name"] = "cuboid-verify"
	if c.Main, err = pgxpool.NewWithConfig(ctx, mcfg); err != nil {
		return nil, err
	}

	c.Audit = audit.New(st.W, c.Log)
	c.Limits = limiter.New(lim)
	c.Hist = cache.NewHistory(1024)
	c.Rec = monitor.NewRecorder(15 * time.Minute)
	c.Alarms = monitor.NewAlarms()
	c.Rebuild = &monitor.Progress{}
	c.flusher = &flush.Flusher{Store: st, Specs: specs, Dict: c.Dict, Loc: c.Loc, Now: time.Now, Log: c.Log, Notify: "cuboid_changes",
		Building: func(name string) bool { return !c.isReady(name) }}
	c.engine = engine.New(cfg, c.Loc, time.Now, c.loadDims)
	p := pipeline.New(c.engine, c.flusher, c.Log)
	p.Interval = opt.FlushInterval
	p.TenantColumn = cfg.Tenancy.TenantColumn
	p.OnFlush = c.onFlush
	p.OnTruncate = func(name string, tables []string) {
		c.Rec.Annotate(fmt.Sprintf("TRUNCATE in %s: reloading it alone", name))
	}
	p.OnEvent = func(kind, sev, detail string) {
		c.Audit.Event(audit.Event{Kind: kind, Severity: sev, Detail: detail})
		if sev == "P1" {
			c.Alarms.Raise(kind, detail)
		}
	}
	c.pipe.Store(p)
	c.Svc = &query.Service{
		Store: st, Dict: c.Dict, Loc: c.Loc, Now: time.Now,
		Cache: cache.New(lim.CacheEntries, c.Hist), Hist: c.Hist, Limits: c.Limits, Members: c.Members,
		Audit: c.Audit, Stats: query.NewStats(), Drill: query.NewDrill(), Latency: monitor.NewLatency(time.Minute),
		Fresh: c.freshness,
	}
	c.Svc.SetActive(c.active(cfg))
	c.Hub = query.NewHub(c.Svc, lim.SSEPerTenant)
	c.Verify = &verify.Verifier{Sources: c.verifySources, Pipeline: c.Pipeline, Svc: c.Svc, Store: st, Loc: c.Loc,
		Limit: rate.NewLimiter(rate.Limit(float64(lim.VerifyPerMinute)/60), 10)}
	c.Verify.OnMismatch = func(tenants []int32, detail string) {
		c.Audit.Event(audit.Event{Kind: "verification_mismatch", Severity: "P1", Detail: detail})
		c.Alarms.Raise("verification_mismatch", "A check against the main database did not match: "+detail)
		// Bonus B-2: a failed check triggers a rebuild, at most once in ten minutes.
		if last := c.lastAutoRebuild.Load(); time.Now().Unix()-last > 600 && c.lastAutoRebuild.CompareAndSwap(last, time.Now().Unix()) {
			c.ReloadStores(tenants, "a check against the main database failed")
		}
	}
	c.Sampler = &verify.Sampler{V: c.Verify, Members: c.Members, Every: 30 * time.Second}
	c.API = &query.API{Svc: c.Svc, Keys: c.Keys, Members: c.Members, Verify: c.Verify.HTTP, Hub: c.Hub}
	return c, nil
}

// active builds the query-side view of a configuration.
func (c *Core) active(cfg *config.Config) *query.Active {
	return &query.Active{
		Cfg:     cfg,
		Planner: &planner.Planner{Cfg: cfg, Loc: c.Loc, Now: time.Now, Ready: c.isReady},
		Parser:  &query.Parser{Cfg: cfg, Loc: c.Loc, Now: time.Now, Dict: c.Dict},
	}
}

func (c *Core) isReady(name string) bool {
	v, ok := c.ready.Load(name)
	return ok && v.(bool)
}

// freshness is how fresh the given stores' data is: the oldest of their
// source databases. tenants nil means every source.
func (c *Core) freshness(tenants []int32) store.Freshness {
	p := c.pipe.Load()
	perStore := c.Config().PerStore()
	c.srcMu.Lock()
	var names []string
	for _, s := range c.sources {
		if tenants == nil || !perStore || (s.tenant != 0 && containsInt32(tenants, s.tenant)) {
			names = append(names, s.name)
		}
	}
	c.srcMu.Unlock()
	var oldest store.Freshness
	for i, name := range names {
		commit, hb := p.AppliedTimes(name)
		f := store.Freshness{LSN: p.Applied(name).String(), CommitTime: commit, Heartbeat: hb}
		if i == 0 || f.AsOf().Before(oldest.AsOf()) {
			oldest = f
		}
	}
	return oldest
}

func (c *Core) loadMembership(ctx context.Context) error {
	tc := c.Config().Tenancy.Company
	if tc == nil {
		return nil
	}
	return c.Members.Load(ctx, c.Store.W, store.QuoteIdent(config.MirrorTable(tc.Table)), store.QuoteIdent(tc.Key), store.QuoteIdent(tc.Column), store.QuoteIdent(tc.NameColumn))
}

// loadDims fetches dimension rows from the mirror for the engine.
func (c *Core) loadDims(keys []engine.DimKey) (map[engine.DimKey]engine.Row, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := map[engine.DimKey]engine.Row{}
	byTable := map[string][]engine.DimKey{}
	for _, k := range keys {
		byTable[k.Table] = append(byTable[k.Table], k)
	}
	cfg := c.Config()
	for table, ks := range byTable {
		spec := c.Specs[table]
		src := cfg.Sources.Get(table)
		if spec == nil || len(src.Key) != 2 {
			continue
		}
		keyCol := src.Key[1]
		tenants := make([]int32, len(ks))
		vals := make([]string, len(ks))
		for i, k := range ks {
			tenants[i], vals[i] = k.Tenant, k.Key
		}
		cols := spec.ColumnNames()
		sel := ""
		for i, col := range cols {
			if i > 0 {
				sel += ", "
			}
			sel += "m." + store.QuoteIdent(col) + "::text"
		}
		tcol := store.QuoteIdent(cfg.Tenancy.TenantColumn)
		// Cast the parameter to the column's type, never the column to text: a
		// cast on the column defeats the primary key and scans the store's rows.
		sql := fmt.Sprintf(`SELECT %s FROM %s m JOIN unnest($1::int[], $2::text[]) AS u(t, k) ON m.%s = u.t AND m.%s = u.k::%s`,
			sel, store.Names{}.Mirror(table), tcol, store.QuoteIdent(keyCol), spec.Type(keyCol))
		rows, err := c.Store.W.Query(ctx, sql, tenants, vals)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			vals := make([]*string, len(cols))
			dest := make([]any, len(cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, err
			}
			r := engine.Row{}
			for i, col := range cols {
				if vals[i] != nil {
					r[col] = *vals[i]
				}
			}
			t, k := r[cfg.Tenancy.TenantColumn], r[keyCol]
			var tenant int32
			fmt.Sscan(t, &tenant)
			out[engine.DimKey{Table: table, Tenant: tenant, Key: k}] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EnsureDemoKeys creates the reproducible demo keys: one per store, one per
// company and one operator key (demo mode only).
func (c *Core) EnsureDemoKeys(ctx context.Context) error {
	for _, t := range c.Members.AllTenants() {
		if err := c.Keys.Save(ctx, c.Store.W, auth.Token(auth.Store, t, auth.DemoSecret(c.Opt.DemoSeed, auth.Store, t)), auth.Store, t); err != nil {
			return err
		}
	}
	for _, co := range c.Members.AllCompanies() {
		if err := c.Keys.Save(ctx, c.Store.W, auth.Token(auth.Company, co, auth.DemoSecret(c.Opt.DemoSeed, auth.Company, co)), auth.Company, co); err != nil {
			return err
		}
	}
	return c.Keys.Save(ctx, c.Store.W, auth.Token(auth.Operator, 0, auth.DemoSecret(c.Opt.DemoSeed, auth.Operator, 0)), auth.Operator, 0)
}

// DemoKey returns the demo key of a scope (demo mode only).
func (c *Core) DemoKey(kind auth.Kind, id int32) string {
	return auth.Token(kind, id, auth.DemoSecret(c.Opt.DemoSeed, kind, id))
}

// Run starts every background loop and streams until ctx ends.
func (c *Core) Run(ctx context.Context) error {
	go c.Audit.Run(ctx)
	go c.Hub.Run(ctx)
	go c.background(ctx)
	go c.Rec.Run(ctx, c.sample)
	go c.Sampler.Run(ctx)
	go c.finishInterruptedBuilds(ctx)
	go c.supervise(ctx)
	return c.pipe.Load().Run(ctx)
}

// finishInterruptedBuilds builds rollups a crash left unbuilt: a reload that
// died mid-build leaves them in the config, not ready, and not served.
func (c *Core) finishInterruptedBuilds(ctx context.Context) {
	for ctx.Err() == nil {
		if c.Pipeline() != nil && c.State() == "streaming" {
			var pending []*config.Rollup
			for _, r := range c.Config().Rollups.Values() {
				if !c.isReady(r.Name) {
					pending = append(pending, r)
				}
			}
			if len(pending) == 0 {
				return
			}
			err := c.RebuildFromMirror(ctx, nil, pending, nil, "finishing a build interrupted by a restart")
			if err == nil {
				return
			}
			c.Log.Warn("finish interrupted build", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (c *Core) onFlush(res *flush.Result) {
	c.Alarms.Clear("flush_failed") // this flush committed, so the failure is over
	c.Hist.Record(res.Versions, res.Touched)
	c.Hub.Notify()
	for _, ev := range res.Rebuilt {
		c.Rec.Annotate("Recategorised: rebuilt " + ev + " from the mirror")
	}
}

// background runs periodic upkeep.
func (c *Core) background(ctx context.Context) {
	fast := time.NewTicker(2 * time.Second)
	slow := time.NewTicker(10 * time.Minute)
	defer fast.Stop()
	defer slow.Stop()
	refresh := func() {
		if err := c.Svc.Stats.Refresh(ctx, c.Store, c.Config()); err != nil && ctx.Err() == nil {
			c.Log.Warn("refresh query statistics", "err", err)
		}
	}
	go refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			c.checkHealth(ctx)
			if err := c.loadMembership(ctx); err != nil {
				c.Log.Warn("membership", "err", err)
			}
			if err := c.Keys.Load(ctx, c.Store.W); err != nil {
				c.Log.Warn("keys", "err", err)
			}
		case <-slow.C:
			refresh()
			c.expireCells(ctx)
		}
	}
}

// checkHealth raises and clears the alarms of Section 8.5.
func (c *Core) checkHealth(ctx context.Context) {
	slots, err := c.Emitter.Slots(ctx, "cuboid_")
	if err == nil {
		// The slot holding the most WAL is the one that matters: every slot
		// shares the server's WAL, so one lagging source holds it for all.
		var worst cdc.SlotHealth
		var worstName string
		for name, h := range slots {
			if !worst.Exists || h.RetainedWAL > worst.RetainedWAL {
				worst, worstName = h, name
			}
		}
		c.Rec.SetSlot(worst)
		if worst.RetainedWAL > 1<<30 {
			c.Alarms.Raise("slot_wal", fmt.Sprintf("Replication slot %s holds %d MB of WAL on the main server (alarm above 1024 MB; capped by max_slot_wal_keep_size).", worstName, worst.RetainedWAL>>20))
		} else {
			c.Alarms.Clear("slot_wal")
		}
		var bad []string
		for name, h := range slots {
			if h.WALStatus != "reserved" && h.WALStatus != "" {
				bad = append(bad, name+" is "+h.WALStatus)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			c.Alarms.Raise("slot_status", "Replication slot wal_status: "+strings.Join(bad, "; ")+".")
		} else {
			c.Alarms.Clear("slot_status")
		}
	}
	f := c.freshness(nil)
	if as := f.AsOf(); !as.IsZero() && c.State() == "streaming" {
		if lag := time.Since(as); lag > 30*time.Second {
			c.Alarms.Raise("lag", fmt.Sprintf("Data lag of the stalest source is %s (alarm above 30 s).", lag.Round(time.Second)))
		} else {
			c.Alarms.Clear("lag")
		}
	}
}

// expireCells deletes hour (and other limited-retention) cells older than
// their retention plus a margin. It touches only the aggregate store.
func (c *Core) expireCells(ctx context.Context) {
	cfg := c.Config()
	for _, r := range cfg.Rollups.Values() {
		for g, d := range r.Retention {
			if d == 0 {
				continue
			}
			cut := r.Since(g, time.Now(), c.Loc).Add(-48 * time.Hour)
			if _, err := c.Store.W.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE rollup = $1 AND grain = $2 AND bucket < $3",
				store.Names{}.Cells(r.Source)), r.ID, int16(g), cut); err != nil {
				c.Log.Warn("expire cells", "rollup", r.Name, "err", err)
			}
		}
	}
}

// catalog reads the source tables' columns and validated CHECK constraints:
// the registry's from the platform's database and, with per-store databases,
// every other table's from the template each store's database is created
// from. It returns the columns of every table, the mirror specs and the
// constraint definitions, by table.
func catalog(ctx context.Context, plain *pgx.Conn, dsn string, cfg *config.Config) (map[string][]string, map[string]*store.Table, map[string][]string, error) {
	all, specs, err := store.Catalog(ctx, plain, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	checks, err := store.Checks(ctx, plain, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	if !cfg.PerStore() {
		return all, specs, checks, nil
	}
	tdsn, err := cdc.WithDatabase(dsn, cfg.Tenancy.Databases.Template)
	if err != nil {
		return nil, nil, nil, err
	}
	// Held only for these two reads: CREATE DATABASE ... TEMPLATE refuses a
	// template that has a connection open.
	tconn, err := cdc.ConnectPlain(ctx, tdsn, "cuboid-setup")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to the store template %s: %w", cfg.Tenancy.Databases.Template, err)
	}
	defer tconn.Close(ctx)
	tall, tspecs, err := store.Catalog(ctx, tconn, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	tchecks, err := store.Checks(ctx, tconn, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, src := range cfg.SourcesOn(false) {
		all[src.Name], checks[src.Name] = tall[src.Name], tchecks[src.Name]
		if t := tspecs[src.Name]; t != nil {
			specs[src.Name] = t
		} else {
			delete(specs, src.Name)
		}
	}
	return all, specs, checks, nil
}
