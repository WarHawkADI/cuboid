package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cuboid/internal/auth"
	"cuboid/internal/config"
	"cuboid/internal/query"
	"cuboid/internal/verify"
)

// Routes returns the full HTTP handler: data API, operator API, monitoring
// state and the web UI.
func (c *Core) Routes() http.Handler {
	mux := http.NewServeMux()
	c.API.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "state": c.State()})
	})
	mux.HandleFunc("GET /metrics", c.prometheus)
	mux.HandleFunc("GET /api/monitor/state", c.monitorState)
	mux.HandleFunc("GET /api/scorecard", c.scorecard)
	mux.HandleFunc("GET /api/bench", c.bench)
	mux.HandleFunc("GET /api/demo/keys", c.demoKeys)
	mux.HandleFunc("POST /admin/rebuild", c.operator(c.adminRebuild))
	mux.HandleFunc("POST /admin/config/reload", c.operator(func(w http.ResponseWriter, r *http.Request) {
		rep := c.Reload(r.Context())
		c.lastReload.Store(rep)
		status := 200
		if !rep.OK {
			status = 422
		}
		writeJSON(w, status, rep)
	}))
	mux.HandleFunc("POST /admin/drill", c.operator(c.adminDrill))
	mux.HandleFunc("POST /admin/flood", c.operator(c.adminFlood))
	mux.HandleFunc("POST /admin/path", c.operator(c.adminPath))
	mux.HandleFunc("POST /admin/scenario", c.operator(c.adminScenario))
	mux.HandleFunc("POST /admin/verify-batch", c.operator(c.adminVerifyBatch))
	mux.HandleFunc("POST /admin/chaos", c.operator(c.adminChaos))
	mux.HandleFunc("POST /admin/annotate", c.operator(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Text string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
		if b.Text != "" {
			c.Rec.Annotate(b.Text)
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	if c.Opt.WebDir != "" {
		mux.Handle("GET /", spa(c.Opt.WebDir))
	}
	return securityHeaders(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// securityHeaders: CSP without inline scripts, no framing, no sniffing, and
// a request size limit on every route.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h.ServeHTTP(w, r)
	})
}

// spa serves the built web UI, falling back to index.html for client routes.
func spa(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fs.ServeHTTP(w, r)
	})
}

func (c *Core) operator(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s, ok := c.Keys.Authenticate(strings.TrimSpace(tok))
		if !ok || s.Kind != auth.Operator {
			query.WriteProblem(w, &query.Problem{Status: 401, Type: "unauthenticated", Title: "Operator key required",
				Detail: "Admin actions need the operator key.", Hint: "Run make keys, or use the demo key picker."})
			return
		}
		h(w, r)
	}
}

// demoKeys lists the reproducible demo keys. Demo mode only, and labelled.
func (c *Core) demoKeys(w http.ResponseWriter, r *http.Request) {
	if !c.Opt.DemoMode {
		http.NotFound(w, r)
		return
	}
	type entry struct {
		Kind  string  `json:"kind"`
		ID    int32   `json:"id"`
		Name  string  `json:"name"`
		Key   string  `json:"key"`
		Store []int32 `json:"stores,omitempty"`
	}
	out := []entry{{Kind: "operator", Name: "Operator", Key: c.DemoKey(auth.Operator, 0)}}
	for _, co := range c.Members.AllCompanies() {
		stores := c.Members.Stores(co)
		out = append(out, entry{Kind: "company", ID: co, Name: fmt.Sprintf("Company %d", co), Key: c.DemoKey(auth.Company, co), Store: stores})
	}
	for _, t := range c.Members.AllTenants() {
		out = append(out, entry{Kind: "store", ID: t, Name: c.Members.Name(t), Key: c.DemoKey(auth.Store, t)})
	}
	writeJSON(w, 200, map[string]any{"demo_mode": true, "note": "Demo keys are derived from CUBOID_DEMO_KEY_SEED so the README works verbatim. Production keys are random.", "keys": out})
}

func (c *Core) adminRebuild(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := q.Get("source")
	scope := q.Get("scope")
	if source == "" {
		source = "mirror"
	}
	if scope == "" {
		scope = "all"
	}
	switch source {
	case "main":
		if scope != "all" {
			query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Invalid scope", Detail: "A rebuild from the main database rebuilds every store; use source=mirror for one store or rollup."})
			return
		}
		c.RebuildFromMain("requested by an operator")
		writeJSON(w, 202, map[string]string{"status": "started", "source": "main"})
	case "mirror":
		var tenants []int32
		var label = "requested by an operator"
		cfg := c.Config()
		rollups := cfg.Rollups.Values()
		switch {
		case strings.HasPrefix(scope, "tenant:"):
			id, err := strconv.Atoi(strings.TrimPrefix(scope, "tenant:"))
			if err != nil {
				query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Invalid scope", Detail: "use scope=all, tenant:<id> or rollup:<name>"})
				return
			}
			tenants = []int32{int32(id)}
		case strings.HasPrefix(scope, "rollup:"):
			ro := cfg.Rollups.Get(strings.TrimPrefix(scope, "rollup:"))
			if ro == nil {
				query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Unknown rollup", Detail: scope})
				return
			}
			rollups = []*config.Rollup{ro}
		case scope != "all":
			query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Invalid scope", Detail: "use scope=all, tenant:<id> or rollup:<name>"})
			return
		}
		go func() {
			if err := c.RebuildFromMirror(context.Background(), tenants, rollups, nil, label); err != nil {
				c.Log.Error("rebuild from mirror", "err", err)
			}
		}()
		writeJSON(w, 202, map[string]string{"status": "started", "source": "mirror", "scope": scope})
	default:
		query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Invalid source", Detail: "use source=mirror or source=main"})
	}
}

func (c *Core) adminPath(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	c.forward(w, c.Opt.LoadgenURL+"/control", body)
}

func (c *Core) adminScenario(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	var b struct {
		Name string `json:"name"`
	}
	json.Unmarshal(body, &b)
	if b.Name != "" {
		c.Rec.Annotate(scenarioNote(b.Name))
	}
	c.forward(w, c.Opt.StoreAppURL+"/control/scenario", body)
}

func scenarioNote(name string) string {
	switch name {
	case "bulk_update":
		return "50,000 orders bulk-updated in one transaction"
	case "surge":
		return "Store surge started"
	case "late_updates":
		return "Late updates to weeks-old orders"
	case "truncate_reload":
		return "Orders truncated and reloaded"
	}
	return "Scenario: " + name
}

func (c *Core) forward(w http.ResponseWriter, url string, body []byte) {
	if url == "" || strings.HasPrefix(url, "/") {
		query.WriteProblem(w, &query.Problem{Status: 503, Type: "unavailable", Title: "Not connected", Detail: "This control needs the companion service, which is not configured."})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		query.WriteProblem(w, &query.Problem{Status: 503, Type: "unavailable", Title: "Not reachable", Detail: err.Error()})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

func (c *Core) adminChaos(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
	switch b.Name {
	case "kill":
		c.Rec.Annotate("Cuboid killed (kill -9)")
		writeJSON(w, 202, map[string]string{"status": "killing"})
		go func() {
			time.Sleep(200 * time.Millisecond)
			p, _ := os.FindProcess(os.Getpid())
			p.Kill() // SIGKILL: no flush, no cleanup; the supervisor restarts us
		}()
	case "drop_replication":
		c.Rec.Annotate("Replication connection dropped")
		if stop := c.streamStop.Load(); stop != nil {
			(*stop)()
		}
		writeJSON(w, 202, map[string]string{"status": "dropped"})
	default:
		query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Unknown chaos action", Detail: "use kill or drop_replication"})
	}
}

func (c *Core) adminVerifyBatch(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 2000 {
		n = 100
	}
	c.batchMu.Lock()
	if c.batch.Running {
		c.batchMu.Unlock()
		writeJSON(w, 409, c.batch)
		return
	}
	c.batch = BatchStatus{Running: true, Total: n, StartedAt: time.Now()}
	c.batchMu.Unlock()
	go c.runBatch(n)
	writeJSON(w, 202, map[string]any{"status": "started", "total": n})
}

// BatchStatus is "Verify everything".
type BatchStatus struct {
	Running    bool             `json:"running"`
	Total      int              `json:"total"`
	Done       int              `json:"done"`
	Exact      int              `json:"exact"`
	Mismatch   int              `json:"mismatch"`
	Failed     int              `json:"failed"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at"`
	Last       []verify.Summary `json:"last,omitempty"`
}

func (c *Core) runBatch(n int) {
	tenants := c.Members.AllTenants()
	companies := c.Members.AllCompanies()
	for i := 0; i < n; i++ {
		var t query.Target
		company := len(companies) > 0 && i%7 == 6
		if company {
			co := companies[i%len(companies)]
			t = verify.OperatorTarget(c.Members.Stores(co), true, co)
		} else {
			tn := tenants[(i*7919)%len(tenants)]
			t = verify.OperatorTarget([]int32{tn}, false, tn)
		}
		req, err := verify.RandomRequest(c.Svc, c.Loc, company)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var res *verify.Result
		if err == nil {
			res, err = c.Verify.Check(ctx, t, req)
		}
		cancel()
		c.batchMu.Lock()
		c.batch.Done++
		switch {
		case err != nil:
			c.batch.Failed++
		case res.Exact:
			c.batch.Exact++
		default:
			c.batch.Mismatch++
		}
		c.batchMu.Unlock()
	}
	c.batchMu.Lock()
	c.batch.Running = false
	c.batch.FinishedAt = time.Now()
	c.batchMu.Unlock()
	c.Rec.Annotate(fmt.Sprintf("Verified %d of %d exact", c.batch.Exact, n))
}

// latestArtifact returns the newest JSON file in artifacts/<kind>.
func (c *Core) latestArtifact(kind string) ([]byte, string, error) {
	dir := filepath.Join(c.Opt.ArtifactsDir, kind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, "", os.ErrNotExist
	}
	sort.Strings(names)
	b, err := os.ReadFile(filepath.Join(dir, names[len(names)-1]))
	return b, names[len(names)-1], err
}

func (c *Core) scorecard(w http.ResponseWriter, r *http.Request) {
	b, name, err := c.latestArtifact("targets")
	if err != nil {
		writeJSON(w, 200, map[string]any{"available": false, "hint": "Run make targets to measure T1 to T5."})
		return
	}
	w.Header().Set("X-Artifact", name)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (c *Core) bench(w http.ResponseWriter, r *http.Request) {
	b, name, err := c.latestArtifact("bench")
	if err != nil {
		writeJSON(w, 200, map[string]any{"available": false, "hint": "Run make bench."})
		return
	}
	w.Header().Set("X-Artifact", name)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// monitorState is everything the monitoring page shows, in one response.
func (c *Core) monitorState(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	points, notes := c.Rec.Snapshot(since)
	f := c.freshness()
	var lag int64 = -1
	if as := f.AsOf(); !as.IsZero() {
		lag = time.Since(as).Milliseconds()
	}
	p := c.Pipeline()
	pipe := map[string]any{}
	if p != nil {
		pipe = map[string]any{"applied_lsn": p.Applied().String(), "flushes": p.Flushes.Load(), "transactions": p.TxnsApplied.Load(), "last_flush_ms": p.LastFlushMS.Load()}
	}
	cfg := c.Config()
	var rollups []map[string]any
	for _, ro := range cfg.Rollups.Values() {
		rollups = append(rollups, map[string]any{"name": ro.Name, "dims": ro.Dims, "ready": c.isReady(ro.Name)})
	}
	c.batchMu.Lock()
	batch := c.batch
	c.batchMu.Unlock()
	hits, misses := c.Svc.Cache.Hits.Load(), c.Svc.Cache.Misses.Load()
	writeJSON(w, 200, map[string]any{
		"now":        time.Now().UnixMilli(),
		"state":      c.State(),
		"started_at": c.StartedAt,
		"demo_mode":  c.Opt.DemoMode,
		"timezone":   c.Loc.String(),
		"freshness":  map[string]any{"lag_ms": lag, "lsn": f.LSN, "as_of": f.AsOf()},
		"slot":       c.Rec.Slot(),
		"pipeline":   pipe,
		"alarms":     c.Alarms.List(),
		"latency":    c.Svc.Latency.Snapshot(),
		"cache":      map[string]any{"hits": hits, "misses": misses, "coalesced": c.Svc.Coalesced.Load()},
		"limiter":    c.Limits.Snapshot(),
		"rate_429":   c.Limits.Rate429.Load(),
		"rebuild":    c.Rebuild.Snapshot(),
		"reload":     c.lastReload.Load(),
		"verify": map[string]any{"checks": c.Verify.Checks.Load(), "exact": c.Verify.Exact.Load(), "mismatch": c.Verify.Mismatch.Load(),
			"recent": first(c.Verify.Recent(), 12), "batch": batch, "sampler": c.Sampler.Enabled.Load()},
		"isolation": map[string]any{"layers": c.Svc.Drill.Counts(), "drill_mode": c.Svc.Drill.Mode().String(),
			"cross_tenant_blocked": c.Svc.CrossTenantBlocked.Load(), "last_drill": c.lastDrill.Load()},
		"flood":    c.lastFlood.Load(),
		"security": last(c.Audit.Recent(), 20),
		"recorder": map[string]any{"points": points, "annotations": notes, "baseline_p95": c.Rec.Baseline()},
		"rollups":  rollups,
		"sse":      map[string]any{"connections": c.Hub.Connections.Load(), "frames": c.Hub.Frames.Load()},
	})
}

func first[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

func last[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func (c *Core) prometheus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	f := c.freshness()
	lag := 0.0
	if as := f.AsOf(); !as.IsZero() {
		lag = time.Since(as).Seconds()
	}
	fmt.Fprintf(w, "# HELP cuboid_data_lag_seconds Time since the newest applied heartbeat or commit.\n# TYPE cuboid_data_lag_seconds gauge\ncuboid_data_lag_seconds %.3f\n", lag)
	fmt.Fprintf(w, "# TYPE cuboid_cross_tenant_blocked_total counter\ncuboid_cross_tenant_blocked_total %d\n", c.Svc.CrossTenantBlocked.Load())
	fmt.Fprintf(w, "# TYPE cuboid_cache_hits_total counter\ncuboid_cache_hits_total %d\n", c.Svc.Cache.Hits.Load())
	fmt.Fprintf(w, "# TYPE cuboid_cache_misses_total counter\ncuboid_cache_misses_total %d\n", c.Svc.Cache.Misses.Load())
	fmt.Fprintf(w, "# TYPE cuboid_verifications_total counter\ncuboid_verifications_total{result=\"exact\"} %d\ncuboid_verifications_total{result=\"mismatch\"} %d\n", c.Verify.Exact.Load(), c.Verify.Mismatch.Load())
	h := c.Rec.Slot()
	fmt.Fprintf(w, "# TYPE cuboid_slot_retained_wal_bytes gauge\ncuboid_slot_retained_wal_bytes %d\n", h.RetainedWAL)
	fmt.Fprintln(w, "# TYPE cuboid_query_seconds summary")
	for _, p := range c.Svc.Latency.Snapshot() {
		fmt.Fprintf(w, "cuboid_query_seconds{endpoint=%q,quantile=\"0.5\"} %.6f\n", p.Name, p.P50/1000)
		fmt.Fprintf(w, "cuboid_query_seconds{endpoint=%q,quantile=\"0.99\"} %.6f\n", p.Name, p.P99/1000)
	}
}
