// Package limiter bounds what any one tenant can ask of the aggregate store
// (bonus B-3): a per-tenant limit on concurrent heavy queries, a separate
// larger pool for light queries, a global cap on store concurrency, and a
// per-key request rate. A noisy store gets 429s; its neighbours never notice.
package limiter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

// Config is config/limits.yaml.
type Config struct {
	HeavyCost         int64   `yaml:"heavy_cost"`          // estimated cells + edge rows above which a query is heavy
	HeavyPerTenant    int64   `yaml:"heavy_per_tenant"`    // concurrent heavy queries per store or company
	LightPerTenant    int64   `yaml:"light_per_tenant"`    // concurrent light queries per store or company
	GlobalConcurrency int64   `yaml:"global_concurrency"`  // concurrent aggregate-store reads overall
	WaitMS            int     `yaml:"wait_ms"`             // how long a query may wait for a slot before 429
	KeyRate           float64 `yaml:"key_rate_per_second"` // per-key token bucket rate
	KeyBurst          int     `yaml:"key_burst"`
	MaxEdgeRows       int64   `yaml:"max_edge_rows"` // estimated mirror rows a query may read
	CacheEntries      int     `yaml:"cache_entries"` // per tenant partition
	VerifyPerMinute   int     `yaml:"verify_per_minute"`
	SSEPerTenant      int     `yaml:"sse_per_tenant"`
}

// Defaults are used for any value the file leaves out.
var Defaults = Config{HeavyCost: 20000, HeavyPerTenant: 2, LightPerTenant: 32, GlobalConcurrency: 16, WaitMS: 200,
	KeyRate: 50, KeyBurst: 100, MaxEdgeRows: 400000, CacheEntries: 512, VerifyPerMinute: 120, SSEPerTenant: 50}

// LoadConfig reads limits.yaml.
func LoadConfig(path string) (Config, error) {
	c := Defaults
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// ErrLimited means a tenant is at its heavy-query limit.
type ErrLimited struct {
	Limit int64
	Heavy bool
}

func (e *ErrLimited) Error() string {
	if e.Heavy {
		return fmt.Sprintf("Too many heavy queries from this store right now (limit %d). Try again in 1 second.", e.Limit)
	}
	return fmt.Sprintf("Too many queries from this store right now (limit %d). Try again in 1 second.", e.Limit)
}

// ErrSaturated means the aggregate store is at its global concurrency cap.
var ErrSaturated = errors.New("the aggregate store is at its concurrency limit")

type tenant struct {
	heavy, light       *semaphore.Weighted
	heavyIn, lightIn   atomic.Int64
	rejected, admitted atomic.Int64
}

// Limiter enforces Config.
type Limiter struct {
	cfg     Config
	global  *semaphore.Weighted
	mu      sync.Mutex
	tenants map[string]*tenant
	keys    map[string]*rate.Limiter
	Rate429 atomic.Int64
}

// New returns a limiter.
func New(cfg Config) *Limiter {
	return &Limiter{cfg: cfg, global: semaphore.NewWeighted(cfg.GlobalConcurrency), tenants: map[string]*tenant{}, keys: map[string]*rate.Limiter{}}
}

// Config returns the active limits.
func (l *Limiter) Config() Config { return l.cfg }

func (l *Limiter) tenant(p string) *tenant {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.tenants[p]
	if t == nil {
		t = &tenant{heavy: semaphore.NewWeighted(l.cfg.HeavyPerTenant), light: semaphore.NewWeighted(l.cfg.LightPerTenant)}
		l.tenants[p] = t
	}
	return t
}

// AllowKey applies the per-key token bucket.
func (l *Limiter) AllowKey(prefix string) bool {
	l.mu.Lock()
	r := l.keys[prefix]
	if r == nil {
		r = rate.NewLimiter(rate.Limit(l.cfg.KeyRate), l.cfg.KeyBurst)
		l.keys[prefix] = r
	}
	l.mu.Unlock()
	if !r.Allow() {
		l.Rate429.Add(1)
		return false
	}
	return true
}

// Acquire takes a slot for a query of partition p. It waits up to WaitMS.
func (l *Limiter) Acquire(ctx context.Context, p string, heavy bool) (func(), error) {
	t := l.tenant(p)
	sem, in, limit := t.light, &t.lightIn, l.cfg.LightPerTenant
	if heavy {
		sem, in, limit = t.heavy, &t.heavyIn, l.cfg.HeavyPerTenant
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(l.cfg.WaitMS)*time.Millisecond)
	defer cancel()
	if err := sem.Acquire(wctx, 1); err != nil {
		t.rejected.Add(1)
		return nil, &ErrLimited{Limit: limit, Heavy: heavy}
	}
	in.Add(1)
	gctx, gcancel := context.WithTimeout(ctx, 2*time.Second)
	defer gcancel()
	if err := l.global.Acquire(gctx, 1); err != nil {
		in.Add(-1)
		sem.Release(1)
		return nil, ErrSaturated
	}
	t.admitted.Add(1)
	return func() {
		l.global.Release(1)
		in.Add(-1)
		sem.Release(1)
	}, nil
}

// State is one partition's limiter state, for the monitoring page.
type State struct {
	Partition string `json:"partition"`
	HeavyIn   int64  `json:"heavy_in_flight"`
	HeavyMax  int64  `json:"heavy_limit"`
	LightIn   int64  `json:"light_in_flight"`
	Rejected  int64  `json:"rejected"`
	Admitted  int64  `json:"admitted"`
}

// Snapshot returns every partition that has been seen, busiest first.
func (l *Limiter) Snapshot() []State {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]State, 0, len(l.tenants))
	for p, t := range l.tenants {
		out = append(out, State{Partition: p, HeavyIn: t.heavyIn.Load(), HeavyMax: l.cfg.HeavyPerTenant,
			LightIn: t.lightIn.Load(), Rejected: t.rejected.Load(), Admitted: t.admitted.Load()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rejected != out[j].Rejected {
			return out[i].Rejected > out[j].Rejected
		}
		return out[i].Admitted > out[j].Admitted
	})
	return out
}
