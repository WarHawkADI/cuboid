package query

import (
	"sync"
	"sync/atomic"
)

// DrillMode is a fault the leak drill injects into Cuboid's own code.
type DrillMode int32

const (
	DrillOff DrillMode = iota
	// DrillSharedCache (A) swaps the partitioned cache for one keyed by the
	// query alone, so stores asking the same question get each other's answers.
	DrillSharedCache
	// DrillConnectionMixup (B) makes the store client hand a request the
	// previous request's rows, as INC-02's pooled connection did.
	DrillConnectionMixup
	// DrillMissingContext (C) forgets the tenant setting on some reads.
	DrillMissingContext
)

func (m DrillMode) String() string {
	return [...]string{"off", "A: shared cache key", "B: connection mix-up", "C: missing tenant setting"}[m]
}

// Layer is one of the six isolation bulkheads.
type Layer int

const (
	LayerKeyScope Layer = iota
	LayerPathCheck
	LayerCachePartition
	LayerRowSecurity
	LayerResultCheck
	LayerResponseGuard
	numLayers
)

// LayerNames are the bulkheads in request order.
var LayerNames = [numLayers]string{"Key scope", "Path check", "Cache partition", "Row security", "Result check", "Response guard"}

// Drill holds the injected fault and the per-layer counters the isolation
// view shows. Counters run all the time, not only during a drill.
type Drill struct {
	mode     atomic.Int32
	mu       sync.Mutex
	shared   map[string]*Response
	last     any
	Injected atomic.Int64
	Checked  [numLayers]atomic.Int64
	Blocked  [numLayers]atomic.Int64
}

// NewDrill returns a drill with no fault active.
func NewDrill() *Drill { return &Drill{shared: map[string]*Response{}} }

// Mode returns the active fault.
func (d *Drill) Mode() DrillMode { return DrillMode(d.mode.Load()) }

// Set activates a fault (or turns it off) and resets drill-only state.
func (d *Drill) Set(m DrillMode) {
	d.mu.Lock()
	d.shared = map[string]*Response{}
	d.last = nil
	d.mu.Unlock()
	d.mode.Store(int32(m))
}

func (d *Drill) injected()        { d.Injected.Add(1) }
func (d *Drill) blocked(l Layer)  { d.Blocked[l].Add(1) }
func (d *Drill) checked(l Layer)  { d.Checked[l].Add(1) }

func (d *Drill) sharedGet(key string) (*Response, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.shared[key]
	if ok {
		d.Injected.Add(1)
	}
	return r, ok
}

func (d *Drill) sharedPut(key string, r *Response) {
	d.mu.Lock()
	d.shared[key] = r
	d.mu.Unlock()
}

// swap stores this request's rows and returns the previous request's.
func (d *Drill) swap(rows any) (any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.last
	d.last = rows
	return prev, prev != nil
}

// LayerCount is one bulkhead's counters.
type LayerCount struct {
	Name    string `json:"name"`
	Checked int64  `json:"checked"`
	Blocked int64  `json:"blocked"`
}

// Counts returns every layer's counters.
func (d *Drill) Counts() []LayerCount {
	out := make([]LayerCount, numLayers)
	for i := range out {
		out[i] = LayerCount{Name: LayerNames[i], Checked: d.Checked[i].Load(), Blocked: d.Blocked[i].Load()}
	}
	return out
}
