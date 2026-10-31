package monitor

import (
	"context"
	"sort"
	"sync"
	"time"

	"cuboid/internal/cdc"
)

// Point is one second of the main database's health, as the PRD measures
// it: the store app's normal query time, plus what is loading the database.
type Point struct {
	T          int64   `json:"t"` // Unix milliseconds
	AppP50     float64 `json:"app_p50"`
	AppP95     float64 `json:"app_p95"`
	AppOps     float64 `json:"app_ops"`
	DirectQPS  float64 `json:"direct_qps"`  // dashboard queries per second reaching the main database
	CuboidQPS  float64 `json:"cuboid_qps"`  // dashboard queries per second served by Cuboid
	MainActive int     `json:"main_active"` // active backends on the main database
	LagMS      int64   `json:"lag_ms"`
	Mode       string  `json:"mode"`    // the dashboards' path: off, direct, direct-cache, cuboid
	Viewers    int     `json:"viewers"` // connected dashboards
}

// Annotation marks an event on the recorder.
type Annotation struct {
	T    int64  `json:"t"`
	Text string `json:"text"`
}

// Recorder keeps the last window of points and annotations.
type Recorder struct {
	mu       sync.Mutex
	window   time.Duration
	points   []Point
	notes    []Annotation
	slot     cdc.SlotHealth
	baseline []float64 // app p95 samples while no dashboards were running
	lastMode string
}

// NewRecorder keeps window of history.
func NewRecorder(window time.Duration) *Recorder { return &Recorder{window: window} }

// Run samples once a second until ctx ends.
func (r *Recorder) Run(ctx context.Context, sample func(context.Context) Point) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
			p := sample(sctx)
			cancel()
			r.add(p)
		}
	}
}

func (r *Recorder) add(p Point) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.T == 0 {
		p.T = time.Now().UnixMilli()
	}
	if r.lastMode != "" && p.Mode != r.lastMode {
		r.notes = append(r.notes, Annotation{T: p.T, Text: modeNote(p.Mode, p.Viewers)})
	}
	r.lastMode = p.Mode
	r.points = append(r.points, p)
	if (p.Mode == "off" || p.Viewers == 0) && p.AppP95 > 0 {
		r.baseline = append(r.baseline, p.AppP95)
		if len(r.baseline) > 120 {
			r.baseline = r.baseline[len(r.baseline)-120:]
		}
	}
	cut := time.Now().Add(-r.window).UnixMilli()
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i].T >= cut })
	r.points = r.points[i:]
	j := sort.Search(len(r.notes), func(i int) bool { return r.notes[i].T >= cut })
	r.notes = r.notes[j:]
}

func modeNote(mode string, viewers int) string {
	switch mode {
	case "direct":
		return "Switched to Direct"
	case "direct-cache":
		return "Switched to Direct + cache"
	case "cuboid":
		return "Switched to Cuboid"
	}
	return "Dashboards stopped"
}

// Annotate marks an event now.
func (r *Recorder) Annotate(text string) {
	r.mu.Lock()
	r.notes = append(r.notes, Annotation{T: time.Now().UnixMilli(), Text: text})
	r.mu.Unlock()
}

// SetSlot records the latest slot health.
func (r *Recorder) SetSlot(h cdc.SlotHealth) {
	r.mu.Lock()
	r.slot = h
	r.mu.Unlock()
}

// Slot returns the latest slot health.
func (r *Recorder) Slot() cdc.SlotHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slot
}

// Baseline is the median app p95 measured with no dashboards running, or 0
// before any such sample exists.
func (r *Recorder) Baseline() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return median(r.baseline)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// AppUnderPressure reports whether the app's recent p95 exceeds the baseline
// by more than factor (the backfill canary).
func (r *Recorder) AppUnderPressure(factor float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := median(r.baseline)
	if b == 0 || len(r.points) < 3 {
		return false
	}
	var recent []float64
	for _, p := range r.points[max(0, len(r.points)-5):] {
		recent = append(recent, p.AppP95)
	}
	return median(recent) > b*factor
}

// Snapshot returns points and annotations after since (Unix ms).
func (r *Recorder) Snapshot(since int64) ([]Point, []Annotation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i].T > since })
	j := sort.Search(len(r.notes), func(i int) bool { return r.notes[i].T > since })
	return append([]Point(nil), r.points[i:]...), append([]Annotation(nil), r.notes[j:]...)
}

// Alarm is an active alarm.
type Alarm struct {
	Name   string    `json:"name"`
	Since  time.Time `json:"since"`
	Detail string    `json:"detail"`
}

// Alarms tracks the active alarms.
type Alarms struct {
	mu     sync.Mutex
	active map[string]*Alarm
}

// NewAlarms returns an empty set.
func NewAlarms() *Alarms { return &Alarms{active: map[string]*Alarm{}} }

// Raise activates an alarm (or updates its detail).
func (a *Alarms) Raise(name, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if x, ok := a.active[name]; ok {
		x.Detail = detail
		return
	}
	a.active[name] = &Alarm{Name: name, Since: time.Now(), Detail: detail}
}

// Clear deactivates an alarm.
func (a *Alarms) Clear(name string) {
	a.mu.Lock()
	delete(a.active, name)
	a.mu.Unlock()
}

// List returns active alarms, oldest first.
func (a *Alarms) List() []Alarm {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Alarm, 0, len(a.active))
	for _, x := range a.active {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Progress is the state of the current or last rebuild.
type Progress struct {
	mu         sync.Mutex
	Kind       string    `json:"kind"` // mirror | main
	Scope      string    `json:"scope"`
	Reason     string    `json:"reason"`
	Phase      string    `json:"phase"`
	Done       int64     `json:"done"`
	Total      int64     `json:"total"`
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Message    string    `json:"message"`
	Error      string    `json:"error,omitempty"`
}

// Start begins a rebuild.
func (p *Progress) Start(kind, scope, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Kind, p.Scope, p.Reason, p.Phase = kind, scope, reason, "starting"
	p.Done, p.Total, p.Running, p.StartedAt, p.FinishedAt, p.Message, p.Error = 0, 0, true, time.Now(), time.Time{}, "", ""
}

// Update records progress within a phase.
func (p *Progress) Update(phase string, done, total int64) {
	p.mu.Lock()
	p.Phase, p.Done, p.Total = phase, done, total
	p.mu.Unlock()
}

// Finish ends a rebuild successfully.
func (p *Progress) Finish(msg string) {
	p.mu.Lock()
	p.Running, p.Phase, p.FinishedAt, p.Message = false, "done", time.Now(), msg
	p.mu.Unlock()
}

// Fail finishes a rebuild with an error and returns it.
func (p *Progress) Fail(err error) error {
	p.mu.Lock()
	p.Running, p.Phase, p.FinishedAt, p.Error = false, "failed", time.Now(), err.Error()
	p.mu.Unlock()
	return err
}

// Snapshot copies the progress.
func (p *Progress) Snapshot() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Progress{Kind: p.Kind, Scope: p.Scope, Reason: p.Reason, Phase: p.Phase, Done: p.Done, Total: p.Total,
		Running: p.Running, StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, Message: p.Message, Error: p.Error}
}
