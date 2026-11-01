// Command naive is the baseline Cuboid is measured against: the same four
// dashboard queries answered with SQL directly on the main database, with
// fair analytics indexes and a 40-connection pool. With caching on, it keeps
// a correct per-store TTL cache (60 s), the "obvious fix".
//
// It uses the verifier's SQL generator, so both paths compute exactly the
// same thing; only where the work happens differs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // the runtime image has no zone database; Asia/Kolkata must load anyway

	"github.com/jackc/pgx/v5/pgxpool"

	"cuboid/internal/config"
	"cuboid/internal/query"
	"cuboid/internal/verify"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type entry struct {
	at   time.Time
	body []byte
}

type server struct {
	dsn    *pgxpool.Config // naive_dash; each store's queries run in its own database
	pools  sync.Map        // database -> *pgxpool.Pool
	cfg    *config.Config
	parser *query.Parser
	loc    *time.Location
	cache  atomic.Bool
	ttl    time.Duration

	mu       sync.Mutex
	entries  map[string]entry
	window   []time.Time
	inFlight atomic.Int64
	queries  atomic.Int64
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load(env("NAIVE_METRICS", "config/metrics.yaml"))
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	pcfg, err := pgxpool.ParseConfig(env("NAIVE_MAIN_DSN", "postgres://naive_dash:naive_dash@localhost:5433/shop"))
	if err != nil {
		log.Error("dsn", "err", err)
		os.Exit(1)
	}
	pcfg.ConnConfig.RuntimeParams["application_name"] = "naive-dashboards"
	loc, _ := time.LoadLocation(env("NAIVE_TZ", "Asia/Kolkata"))
	s := &server{dsn: pcfg, cfg: cfg, loc: loc, ttl: 60 * time.Second, entries: map[string]entry{},
		parser: &query.Parser{Cfg: cfg, Loc: loc, Now: time.Now}}
	s.cache.Store(env("NAIVE_CACHE", "false") == "true")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tenants/{id}/{name}/{op}", s.query)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("POST /control", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Cache *bool `json:"cache"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		if b.Cache != nil {
			s.cache.Store(*b.Cache)
			s.mu.Lock()
			s.entries = map[string]entry{}
			s.mu.Unlock()
		}
		writeJSON(w, 200, map[string]bool{"cache": s.cache.Load()})
	})
	srv := &http.Server{Addr: env("NAIVE_LISTEN", ":8083"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("direct baseline serving", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	<-ctx.Done()
	srv.Shutdown(context.Background())
}

// poolFor returns the pool of a store's own database (ADR-012), or of the
// one database every store shares when the config has no per-store databases.
// Forty connections in all, as a careful team would give a shared database.
func (s *server) poolFor(ctx context.Context, tenant int32) (*pgxpool.Pool, error) {
	db := s.dsn.ConnConfig.Database
	conns := int32(40)
	if s.cfg.PerStore() {
		db, conns = s.cfg.StoreDatabase(tenant), 4
	}
	if p, ok := s.pools.Load(db); ok {
		return p.(*pgxpool.Pool), nil
	}
	cfg := s.dsn.Copy()
	cfg.ConnConfig.Database = db
	cfg.MaxConns = conns
	cfg.MaxConnIdleTime = time.Minute
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if actual, loaded := s.pools.LoadOrStore(db, p); loaded {
		p.Close()
		return actual.(*pgxpool.Pool), nil
	}
	return p, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *server) query(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	name, op := r.PathValue("name"), r.PathValue("op")
	var src *config.Source
	var tl *config.TopList
	if op == "top" {
		for _, f := range s.cfg.FactSources() {
			if t := f.Top.Get(name); t != nil {
				src, tl = f, t
			}
		}
	} else {
		src = s.cfg.Sources.Get(name)
	}
	if src == nil {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	req, prob := s.parser.Parse(op, src, tl, r.URL.Query(), false)
	if prob != nil {
		writeJSON(w, prob.Status, prob)
		return
	}
	key := fmt.Sprintf("%d|%s|%s", id, r.URL.Path, req.Canonical)
	if req.Range != "" {
		key = fmt.Sprintf("%d|%s|%s", id, r.URL.Path, r.URL.RawQuery) // presets move with the clock; cache by name
	}
	if s.cache.Load() {
		s.mu.Lock()
		e, ok := s.entries[key]
		s.mu.Unlock()
		if ok && time.Since(e.at) < s.ttl {
			w.Header().Set("X-Cache", "hit")
			w.Header().Set("Content-Type", "application/json")
			w.Write(e.body)
			return
		}
	}
	q := req.Query
	q.Tenants = []int32{int32(id)}
	sql, args := verify.DirectSQL(s.cfg, &q, s.loc)
	s.inFlight.Add(1)
	start := time.Now()
	pool, err := s.poolFor(r.Context(), int32(id))
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		s.inFlight.Add(-1)
		return
	}
	rows, err := pool.Query(r.Context(), sql, args...)
	var n int
	if err == nil {
		for rows.Next() {
			n++
		}
		rows.Close()
		err = rows.Err()
	}
	s.inFlight.Add(-1)
	s.queries.Add(1)
	s.mu.Lock()
	s.window = append(s.window, time.Now())
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}
	body, _ := json.Marshal(map[string]any{"tenant_id": id, "rows": n, "took_ms": float64(time.Since(start).Microseconds()) / 1000})
	if s.cache.Load() {
		s.mu.Lock()
		s.entries[key] = entry{at: time.Now(), body: body}
		s.mu.Unlock()
	}
	w.Header().Set("X-Cache", "miss")
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// stats reports how many dashboard queries per second reach the main database.
func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cut := time.Now().Add(-3 * time.Second)
	i := 0
	for i < len(s.window) && s.window[i].Before(cut) {
		i++
	}
	s.window = s.window[i:]
	qps := float64(len(s.window)) / 3
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"qps": qps, "in_flight": s.inFlight.Load(), "queries": s.queries.Load(), "cache": s.cache.Load()})
}
