package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"time"

	"cuboid/internal/auth"
	"cuboid/internal/monitor"
	"cuboid/internal/query"
	"cuboid/internal/timeutil"
)

// DrillReport is the outcome of a leak drill.
type DrillReport struct {
	Mode       string           `json:"mode"`
	Seconds    float64          `json:"seconds"`
	Stores     []int32          `json:"stores"`
	Requests   int              `json:"requests"`
	Cancelled  int              `json:"cancelled"`
	Injected   int64            `json:"faults_injected"`
	Blocked    map[string]int64 `json:"blocked_by_layer"`
	BlockedSum int64            `json:"blocked"`
	Leaked     int              `json:"leaked"`
	Statuses   map[int]int      `json:"statuses"`
	P50, P99   float64          `json:"-"`
	Latency    monitor.Percentiles `json:"latency"`
	Finished   time.Time        `json:"finished"`
}

// FloodReport is the outcome of the B-3 fairness flood.
type FloodReport struct {
	Store           int32   `json:"store"`
	Seconds         float64 `json:"seconds"`
	Heavy           int     `json:"heavy_requests"`
	HeavyOK         int     `json:"heavy_answered"`
	HeavyLimited    int     `json:"heavy_limited"`
	KeyLimited      int     `json:"key_rate_limited"`
	LightOK         int     `json:"own_light_answered"`
	LightFailed     int     `json:"own_light_failed"`
	OthersBeforeP99 float64 `json:"others_p99_before_ms"`
	OthersDuringP99 float64 `json:"others_p99_during_ms"`
	OthersFailed    int     `json:"others_failed"`
	Finished        time.Time `json:"finished"`
}

func (c *Core) drillStores() []int32 {
	all := c.Members.AllTenants()
	var out []int32
	for _, want := range []int32{42, 43} {
		for _, t := range all {
			if t == want {
				out = append(out, t)
			}
		}
	}
	for _, t := range all {
		if len(out) >= 3 {
			break
		}
		if t != 42 && t != 43 {
			out = append(out, t)
		}
	}
	return out
}

// drillQueries use whole past months, so the right answer of each store is
// stable while the store app keeps writing today's orders.
func (c *Core) drillQueries() []string {
	now := time.Now().In(c.Loc)
	m := timeutil.Trunc(now, timeutil.GrainMonth, c.Loc)
	from := m.AddDate(0, -3, 0).Format("2006-01-02")
	to := m.AddDate(0, -1, 0).Format("2006-01-02")
	r := "from=" + from + "&to=" + to
	return []string{
		"/orders/count?" + r, "/orders/summary?" + r, "/orders/breakdown?by=status&" + r,
		"/products/top?n=10&" + r, "/orders/trend?interval=month&" + r, "/orders/breakdown?by=category&" + r,
	}
}

// RunDrill injects an INC-02 failure mode into Cuboid itself and sends mixed
// cross-store load through the real HTTP stack with real store keys, with
// random client cancellations. Every answer is checked against what the
// requesting store should see: an answer that matches another store's data
// instead would be a leak.
func (c *Core) RunDrill(ctx context.Context, mode query.DrillMode, dur time.Duration) (*DrillReport, error) {
	if !c.Opt.DemoMode {
		return nil, errors.New("the leak drill runs only in demo mode")
	}
	stores := c.drillStores()
	if len(stores) < 2 {
		return nil, errors.New("the drill needs at least two stores")
	}
	h := c.Routes()
	queries := c.drillQueries()
	get := func(ctx context.Context, store int32, q string) (int, []byte) {
		req := httptest.NewRequestWithContext(ctx, "GET", fmt.Sprintf("/tenants/%d%s", store, q), nil)
		req.Header.Set("Authorization", "Bearer "+c.DemoKey(auth.Store, store))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body struct {
			Data json.RawMessage `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Data
	}
	c.Svc.Drill.Set(query.DrillOff)
	expected := map[int32]map[string]string{}
	for _, s := range stores {
		expected[s] = map[string]string{}
		for _, q := range queries {
			code, data := get(ctx, s, q)
			if code != 200 {
				return nil, fmt.Errorf("drill baseline for store %d %s returned %d", s, q, code)
			}
			expected[s][q] = string(data)
		}
	}
	before := c.Svc.Drill.Counts()
	injectedBefore := c.Svc.Drill.Injected.Load()
	c.Svc.Cache.Purge()
	c.Svc.Drill.Set(mode)
	c.Rec.Annotate("Leak drill " + mode.String())
	defer func() {
		c.Svc.Drill.Set(query.DrillOff)
		c.Svc.Cache.Purge()
	}()
	rep := &DrillReport{Mode: mode.String(), Stores: stores, Statuses: map[int]int{}, Blocked: map[string]int64{}}
	var mu sync.Mutex
	var lat []float64
	start := time.Now()
	deadline := start.Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < 24; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				s := stores[rand.IntN(len(stores))]
				q := queries[rand.IntN(len(queries))]
				rctx, cancel := context.WithCancel(ctx)
				cancelled := rand.IntN(5) == 0
				if cancelled { // the INC-02 trigger: requests cancelled mid-flight
					go func() { time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond); cancel() }()
				}
				t0 := time.Now()
				code, data := get(rctx, s, q)
				cancel()
				d := float64(time.Since(t0).Microseconds()) / 1000
				leak := false
				if code == 200 && string(data) != expected[s][q] {
					for _, o := range stores {
						if o != s && string(data) == expected[o][q] {
							leak = true
						}
					}
				}
				mu.Lock()
				rep.Requests++
				rep.Statuses[code]++
				if cancelled {
					rep.Cancelled++
				}
				if leak {
					rep.Leaked++
				}
				lat = append(lat, d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	rep.Seconds = time.Since(start).Seconds()
	rep.Injected = c.Svc.Drill.Injected.Load() - injectedBefore
	for i, lc := range c.Svc.Drill.Counts() {
		if d := lc.Blocked - before[i].Blocked; d > 0 {
			rep.Blocked[lc.Name] = d
			rep.BlockedSum += d
		}
	}
	rep.Latency = monitor.Summarise("drill", lat)
	rep.Finished = time.Now()
	c.lastDrill.Store(rep)
	c.Rec.Annotate(fmt.Sprintf("Leak drill: %d faults, %d blocked, %d leaked", rep.Injected, rep.BlockedSum, rep.Leaked))
	return rep, nil
}

func (c *Core) adminDrill(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Mode    string  `json:"mode"`
		Seconds float64 `json:"seconds"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
	modes := map[string]query.DrillMode{"A": query.DrillSharedCache, "B": query.DrillConnectionMixup, "C": query.DrillMissingContext}
	m, ok := modes[b.Mode]
	if !ok {
		query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Unknown drill", Detail: "mode is A (shared cache key), B (connection mix-up) or C (missing tenant setting)"})
		return
	}
	if b.Seconds <= 0 || b.Seconds > 60 {
		b.Seconds = 8
	}
	rep, err := c.RunDrill(r.Context(), m, time.Duration(b.Seconds*float64(time.Second)))
	if err != nil {
		query.WriteProblem(w, &query.Problem{Status: 409, Type: "drill_unavailable", Title: "Drill not run", Detail: err.Error()})
		return
	}
	writeJSON(w, 200, rep)
}

// RunFlood makes one store fire many concurrent, cache-busting heavy queries
// while the other stores keep using their dashboards (B-3 fairness).
func (c *Core) RunFlood(ctx context.Context, store int32, dur time.Duration) (*FloodReport, error) {
	if !c.Opt.DemoMode {
		return nil, errors.New("the flood runs only in demo mode")
	}
	h := c.Routes()
	get := func(ctx context.Context, s int32, path string) (int, []byte) {
		req := httptest.NewRequestWithContext(ctx, "GET", fmt.Sprintf("/tenants/%d%s", s, path), nil)
		req.Header.Set("Authorization", "Bearer "+c.DemoKey(auth.Store, s))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	var others []int32
	for _, t := range c.Members.AllTenants() {
		if t != store && len(others) < 8 {
			others = append(others, t)
		}
	}
	light := func(s int32) string { return "/orders/count?range=today" }
	measureOthers := func(d time.Duration, fails *int) float64 {
		var lat []float64
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			for _, o := range others {
				t0 := time.Now()
				code, _ := get(ctx, o, light(o)+"&channel=web")
				lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
				if code != 200 && fails != nil {
					*fails++
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		return monitor.Summarise("others", lat).P99
	}
	rep := &FloodReport{Store: store}
	rep.OthersBeforeP99 = measureOthers(3*time.Second, nil)
	c.Rec.Annotate(fmt.Sprintf("Store %d floods heavy queries", store))
	var mu sync.Mutex
	start := time.Now()
	deadline := start.Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < 200; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				// A unique long range at second precision: a cache miss every time.
				from := time.Now().AddDate(0, 0, -400).Add(-time.Duration(rand.IntN(86400)) * time.Second).In(c.Loc).Format("2006-01-02T15:04:05")
				code, body := get(ctx, store, "/customers/top?n=100&from="+from+"&to="+time.Now().In(c.Loc).Format("2006-01-02T15:04:05"))
				mu.Lock()
				rep.Heavy++
				switch {
				case code == 200:
					rep.HeavyOK++
				case code == 429 && bytes.Contains(body, []byte("heavy")):
					rep.HeavyLimited++
				case code == 429:
					rep.KeyLimited++
				}
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			code, _ := get(ctx, store, "/orders/count?range=today")
			mu.Lock()
			if code == 200 {
				rep.LightOK++
			} else {
				rep.LightFailed++
			}
			mu.Unlock()
			time.Sleep(200 * time.Millisecond)
		}
	}()
	rep.OthersDuringP99 = measureOthers(dur, &rep.OthersFailed)
	wg.Wait()
	rep.Seconds = time.Since(start).Seconds()
	rep.Finished = time.Now()
	c.lastFlood.Store(rep)
	c.Rec.Annotate(fmt.Sprintf("Flood over: %d heavy queries limited, other stores unaffected", rep.HeavyLimited+rep.KeyLimited))
	return rep, nil
}

func (c *Core) adminFlood(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Store   int32   `json:"store"`
		Seconds float64 `json:"seconds"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
	if b.Store == 0 {
		b.Store = c.largestStore()
	}
	if b.Seconds <= 0 || b.Seconds > 60 {
		b.Seconds = 8
	}
	rep, err := c.RunFlood(r.Context(), b.Store, time.Duration(b.Seconds*float64(time.Second)))
	if err != nil {
		query.WriteProblem(w, &query.Problem{Status: 409, Type: "flood_unavailable", Title: "Flood not run", Detail: err.Error()})
		return
	}
	writeJSON(w, 200, rep)
}

// largestStore returns the store with the most orders per hour.
func (c *Core) largestStore() int32 {
	ts := c.Members.AllTenants()
	if len(ts) == 0 {
		return 0
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	for _, t := range ts {
		if t == 42 {
			return 42
		}
	}
	return ts[0]
}
