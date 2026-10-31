// Command loadgen simulates store owners' dashboards. Each viewer belongs to
// a store (more viewers for bigger stores, at least one per store) and
// refreshes a dashboard of the PRD's four example queries plus two
// variations every five seconds, against one of the paths:
//
//	off            no dashboards
//	direct         SQL on the main database (cmd/naive)
//	direct-cache   the same with a 60 s per-store cache
//	cuboid         Cuboid's API
//
// The path can be switched live through POST /control.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cuboid/internal/monitor"
)

var dashboard = []string{
	"/orders/count?range=last_7d",                 // FR-1
	"/orders/trend?interval=day&range=last_30d",   // FR-2
	"/products/top?n=10&by=sales&range=this_week", // FR-3
	"/orders/breakdown?by=status&range=last_7d",   // FR-4
	"/orders/summary?range=today",
	"/orders/breakdown?by=category&range=last_30d",
}

type store struct {
	id  int32
	key string
	w   float64
}

type gen struct {
	log       *slog.Logger
	cuboidURL string
	naiveURL  string
	client    *http.Client

	mu      sync.Mutex
	setMu   sync.Mutex // one path switch at a time, so the path and the baseline's cache agree
	mode    string
	viewers int
	cancel  context.CancelFunc
	stores  []store
	samples []sample
	errs    atomic.Int64
	bad     map[string]int64 // failures by status and query, to say what failed
	reqs    atomic.Int64
	gen     atomic.Int64
}

type sample struct {
	at time.Time
	ms float64
	q  int
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g := &gen{log: log, mode: "off", cuboidURL: env("LOADGEN_CUBOID_URL", "http://localhost:8080"), naiveURL: env("LOADGEN_NAIVE_URL", "http://localhost:8083"),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxConnsPerHost: 256, MaxIdleConns: 512, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", g.state)
	mux.HandleFunc("POST /control", g.control)
	srv := &http.Server{Addr: env("LOADGEN_LISTEN", ":8082"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("load generator serving", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	if m := env("LOADGEN_MODE", ""); m != "" {
		v := 1000
		fmt.Sscan(env("LOADGEN_VIEWERS", "1000"), &v)
		g.set(ctx, m, v)
	}
	<-ctx.Done()
	srv.Shutdown(context.Background())
}

// loadStores reads the demo keys and each store's size (operator key).
func (g *gen) loadStores(ctx context.Context) error {
	var keys struct {
		Keys []struct {
			Kind string `json:"kind"`
			ID   int32  `json:"id"`
			Key  string `json:"key"`
		} `json:"keys"`
	}
	resp, err := g.client.Get(g.cuboidURL + "/api/demo/keys")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return err
	}
	var op string
	var stores []store
	for _, k := range keys.Keys {
		switch k.Kind {
		case "operator":
			op = k.Key
		case "store":
			stores = append(stores, store{id: k.ID, key: k.Key})
		}
	}
	for i := range stores {
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/tenants/%d/orders/count?range=last_30d", g.cuboidURL, stores[i].id), nil)
		req.Header.Set("Authorization", "Bearer "+op)
		var body struct {
			Data struct {
				Count float64 `json:"count"`
			} `json:"data"`
		}
		if r, err := g.client.Do(req); err == nil {
			json.NewDecoder(r.Body).Decode(&body)
			r.Body.Close()
		}
		stores[i].w = body.Data.Count + 1
	}
	if len(stores) == 0 {
		return errors.New("no stores")
	}
	g.mu.Lock()
	g.stores = stores
	g.mu.Unlock()
	return nil
}

// assign gives every store at least one viewer and the rest by size.
func (g *gen) assign(n int) []store {
	g.mu.Lock()
	stores := append([]store(nil), g.stores...)
	g.mu.Unlock()
	if len(stores) == 0 {
		return nil
	}
	out := append([]store(nil), stores...)
	var total float64
	for _, s := range stores {
		total += s.w
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].w > stores[j].w })
	for len(out) < n {
		x := rand.Float64() * total
		for _, s := range stores {
			x -= s.w
			if x <= 0 {
				out = append(out, s)
				break
			}
		}
	}
	return out[:max(n, len(stores))]
}

func (g *gen) set(ctx context.Context, mode string, viewers int) error {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	switch mode {
	case "off", "direct", "direct-cache", "cuboid":
	default:
		return fmt.Errorf("mode is off, direct, direct-cache or cuboid")
	}
	g.mu.Lock()
	running, known := g.mode != "off", len(g.stores) > 0
	g.mu.Unlock()
	// Between two running paths the stores are already known, and re-reading them
	// would stall the switch while Direct has the machine swamped.
	if mode != "off" && !(running && known) {
		if err := g.loadStores(ctx); err != nil {
			return fmt.Errorf("load stores from Cuboid: %w", err)
		}
	}
	if mode == "direct" || mode == "direct-cache" {
		body := fmt.Sprintf(`{"cache": %v}`, mode == "direct-cache")
		if r, err := g.client.Post(g.naiveURL+"/control", "application/json", stringsReader(body)); err == nil {
			r.Body.Close()
		}
	}
	g.mu.Lock()
	if g.cancel != nil {
		g.cancel()
	}
	g.mode, g.viewers = mode, viewers
	vctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	g.mu.Unlock()
	if mode == "off" {
		g.mu.Lock()
		g.viewers = 0
		g.mu.Unlock()
		return nil
	}
	for i, s := range g.assign(viewers) {
		go g.viewer(vctx, mode, s, time.Duration(i%5000)*time.Millisecond)
	}
	g.log.Info("dashboards", "mode", mode, "viewers", viewers)
	return nil
}

type rdr struct {
	s string
	i int
}

func (r *rdr) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func stringsReader(s string) io.Reader { return &rdr{s: s} }

func (g *gen) viewer(ctx context.Context, mode string, s store, offset time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(offset): // spread the first refreshes over five seconds
	}
	for ctx.Err() == nil {
		next := time.Now().Add(5 * time.Second)
		for qi, q := range dashboard {
			var url string
			if mode == "cuboid" {
				url = fmt.Sprintf("%s/tenants/%d%s", g.cuboidURL, s.id, q)
			} else {
				url = fmt.Sprintf("%s/tenants/%d%s", g.naiveURL, s.id, q)
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			if mode == "cuboid" {
				req.Header.Set("Authorization", "Bearer "+s.key)
			}
			start := time.Now()
			resp, err := g.client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					err = fmt.Errorf("status %d", resp.StatusCode)
				}
			}
			if ctx.Err() != nil {
				return
			}
			g.reqs.Add(1)
			if err != nil {
				g.errs.Add(1)
				g.mu.Lock()
				if g.bad == nil {
					g.bad = map[string]int64{}
				}
				g.bad[err.Error()+" "+q]++
				g.mu.Unlock()
				continue
			}
			g.mu.Lock()
			g.samples = append(g.samples, sample{at: time.Now(), ms: float64(time.Since(start).Microseconds()) / 1000, q: qi})
			g.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
	}
}

func (g *gen) state(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	cut := time.Now().Add(-5 * time.Second)
	i := sort.Search(len(g.samples), func(i int) bool { return g.samples[i].at.After(cut) })
	g.samples = g.samples[i:]
	var all []float64
	per := map[int][]float64{}
	for _, s := range g.samples {
		all = append(all, s.ms)
		per[s.q] = append(per[s.q], s.ms)
	}
	mode, viewers := g.mode, g.viewers
	bad := make(map[string]int64, len(g.bad))
	for k, v := range g.bad {
		bad[k] = v
	}
	g.mu.Unlock()
	p := monitor.Summarise("dashboard", all)
	var perQ []monitor.Percentiles
	for qi, v := range per {
		perQ = append(perQ, monitor.Summarise(dashboard[qi], v))
	}
	sort.Slice(perQ, func(i, j int) bool { return perQ[i].Name < perQ[j].Name })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"mode": mode, "viewers": viewers, "qps": float64(p.Count) / 5, "p50_ms": p.P50, "p95_ms": p.P95, "p99_ms": p.P99,
		"requests": g.reqs.Load(), "errors": g.errs.Load(), "failures": bad, "per_query": perQ})
}

func (g *gen) control(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Mode    string `json:"mode"`
		Viewers int    `json:"viewers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if b.Viewers <= 0 {
		b.Viewers = 1000
	}
	if err := g.set(r.Context(), b.Mode, b.Viewers); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	g.state(w, r)
}
