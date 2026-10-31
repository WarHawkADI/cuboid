// Package monitor collects what the monitoring page (bonus B-5) shows:
// query time per endpoint, data lag, slot health, cache and limiter state,
// rebuilds, verification, and the main database's pressure over time.
package monitor

import (
	"sort"
	"sync"
	"time"
)

// Latency keeps recent request durations per name, for percentiles over a
// sliding window.
type Latency struct {
	mu     sync.Mutex
	window time.Duration
	series map[string][]sample
}

type sample struct {
	at time.Time
	ms float64
}

// NewLatency keeps samples for window.
func NewLatency(window time.Duration) *Latency {
	return &Latency{window: window, series: map[string][]sample{}}
}

// Observe records one duration.
func (l *Latency) Observe(name string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := append(l.series[name], sample{at: time.Now(), ms: float64(d.Microseconds()) / 1000})
	if len(s) > 20000 {
		s = s[len(s)-20000:]
	}
	l.series[name] = s
}

// Percentiles is a latency summary in milliseconds.
type Percentiles struct {
	Name  string  `json:"name"`
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

// Snapshot returns percentiles for every name over the window.
func (l *Latency) Snapshot() []Percentiles {
	l.mu.Lock()
	cut := time.Now().Add(-l.window)
	var out []Percentiles
	for name, s := range l.series {
		i := sort.Search(len(s), func(i int) bool { return s[i].at.After(cut) })
		s = s[i:]
		l.series[name] = s
		if len(s) == 0 {
			continue
		}
		v := make([]float64, len(s))
		for i, x := range s {
			v[i] = x.ms
		}
		out = append(out, Summarise(name, v))
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Summarise computes percentiles of values (milliseconds). It sorts values.
func Summarise(name string, v []float64) Percentiles {
	sort.Float64s(v)
	at := func(q float64) float64 {
		if len(v) == 0 {
			return 0
		}
		i := int(q*float64(len(v)-1) + 0.5)
		return v[i]
	}
	p := Percentiles{Name: name, Count: len(v), P50: at(0.50), P95: at(0.95), P99: at(0.99)}
	if len(v) > 0 {
		p.Max = v[len(v)-1]
	}
	return p
}
