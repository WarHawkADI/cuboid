// Package cache holds query answers per tenant. Each tenant (or company) has
// its own partition with its own entry quota, so one tenant can neither
// evict nor read another's entries, and every entry is keyed by the data
// versions it was computed at. An entry stays valid across new versions
// whose changes fall outside its time range: "last 90 days" stays cached
// while today's orders churn.
package cache

import (
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"cuboid/internal/engine"
)

// History records, per tenant, which time ranges each data version changed.
type History struct {
	mu   sync.RWMutex
	byT  map[int32]*hist
	keep int
}

type hist struct {
	version int64
	changes []change // oldest first, at most keep
}

type change struct {
	version int64
	touch   engine.Touch
}

// NewHistory keeps the last keep changes per tenant.
func NewHistory(keep int) *History { return &History{byT: map[int32]*hist{}, keep: keep} }

// Record notes the versions a flush produced and what each touched.
func (h *History) Record(versions map[int32]int64, touched map[int32]*engine.Touch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for t, v := range versions {
		x := h.byT[t]
		if x == nil {
			x = &hist{}
			h.byT[t] = x
		}
		x.version = v
		tc := engine.Touch{All: true}
		if touched[t] != nil {
			tc = *touched[t]
		}
		x.changes = append(x.changes, change{version: v, touch: tc})
		if len(x.changes) > h.keep {
			x.changes = x.changes[len(x.changes)-h.keep:]
		}
	}
}

// Invalidate marks every range of the given tenants changed (after a
// rebuild cut-over), by recording an all-range change at a new version.
func (h *History) Invalidate(tenants []int32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range tenants {
		x := h.byT[t]
		if x == nil {
			x = &hist{}
			h.byT[t] = x
		}
		x.version++
		x.changes = append(x.changes, change{version: x.version, touch: engine.Touch{All: true}})
	}
}

// Version returns a tenant's current data version.
func (h *History) Version(t int32) int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if x := h.byT[t]; x != nil {
		return x.version
	}
	return 0
}

// Unchanged reports whether no change after version v touched [from, to).
func (h *History) Unchanged(t int32, v int64, from, to time.Time) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	x := h.byT[t]
	if x == nil {
		return v == 0
	}
	if x.version == v {
		return true
	}
	if len(x.changes) == 0 || x.changes[0].version > v+1 {
		return false // history no longer reaches back to v
	}
	for _, c := range x.changes {
		if c.version <= v {
			continue
		}
		if c.touch.All || (!c.touch.Max.Before(from) && c.touch.Min.Before(to)) {
			return false
		}
	}
	return true
}

// Entry is one cached answer.
type Entry struct {
	Versions map[int32]int64
	From, To time.Time
	Value    any
	Tenants  []int32   // tenant ids taken from the result rows
	Expires  time.Time // zero: no time limit
	Created  time.Time
}

// Cache is the partitioned store.
type Cache struct {
	mu     sync.Mutex
	parts  map[string]*lru.Cache[string, *Entry]
	quota  int
	hist   *History
	Hits   atomic.Int64
	Misses atomic.Int64
}

// New returns a cache with quota entries per partition.
func New(quota int, h *History) *Cache {
	return &Cache{parts: map[string]*lru.Cache[string, *Entry]{}, quota: quota, hist: h}
}

func (c *Cache) part(p string) *lru.Cache[string, *Entry] {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.parts[p]
	if l == nil {
		l, _ = lru.New[string, *Entry](c.quota)
		c.parts[p] = l
	}
	return l
}

// Get returns a valid entry from partition p.
func (c *Cache) Get(p, key string) (*Entry, bool) {
	e, ok := c.part(p).Get(key)
	if !ok || !c.Valid(e) {
		c.Misses.Add(1)
		return nil, false
	}
	c.Hits.Add(1)
	return e, true
}

// Peek returns an entry from partition p whether or not it is still valid.
func (c *Cache) Peek(p, key string) (*Entry, bool) { return c.part(p).Peek(key) }

// Valid reports whether an entry still matches the data.
func (c *Cache) Valid(e *Entry) bool {
	if !e.Expires.IsZero() && time.Now().After(e.Expires) {
		return false
	}
	for t, v := range e.Versions {
		if !c.hist.Unchanged(t, v, e.From, e.To) {
			return false
		}
	}
	return true
}

// Put stores an entry in partition p.
func (c *Cache) Put(p, key string, e *Entry) { c.part(p).Add(key, e) }

// Purge empties every partition (used by the leak drill's reset).
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.parts = map[string]*lru.Cache[string, *Entry]{}
}

// Snapshot returns the current versions of tenants, for keying an entry.
func (h *History) Snapshot(tenants []int32) map[int32]int64 {
	out := make(map[int32]int64, len(tenants))
	for _, t := range tenants {
		out[t] = h.Version(t)
	}
	return out
}
