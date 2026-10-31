package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cuboid/internal/monitor"
)

// The query set of T1 and T5: the PRD's examples and their variations.
var speedQueries = []struct {
	Name string
	Path string
}{
	{"FR-1 count, last 7 days", "/orders/count?range=last_7d"},
	{"FR-1 summary, last 7 days", "/orders/summary?range=last_7d"},
	{"FR-2 trend per day, last 30 days", "/orders/trend?interval=day&range=last_30d"},
	{"FR-3 top 10 products, this week", "/products/top?n=10&by=sales&range=this_week"},
	{"FR-4 by status, last 7 days", "/orders/breakdown?by=status&range=last_7d"},
	{"FR-4 by category, last 30 days", "/orders/breakdown?by=category&range=last_30d"},
	{"FR-3 top 10 customers, last 30 days", "/customers/top?n=10&by=orders&range=last_30d"},
	{"FR-1 count, arbitrary timestamps", ""},
}

type speedRow struct {
	Query      string              `json:"query"`
	Uncached   monitor.Percentiles `json:"uncached"`
	Cached     monitor.Percentiles `json:"cached"`
	WorstStore int32               `json:"worst_store"`
	Largest    monitor.Percentiles `json:"largest_store_uncached"`
}

type speedArtifact struct {
	Tier        string            `json:"tier"`
	GeneratedAt time.Time         `json:"generated_at"`
	Environment map[string]string `json:"environment"`
	Stores      int               `json:"stores"`
	Rows        []speedRow        `json:"rows"`
	Viewers     int               `json:"viewers"`
	ViewersP50  float64           `json:"viewers_p50_ms"`
	ViewersP99  float64           `json:"viewers_p99_ms"`
	ViewersErr  int64             `json:"viewers_errors"`
}

func arbitrary() string {
	end := time.Now().Add(-time.Duration(rand.IntN(30*24*3600)) * time.Second).Truncate(time.Second)
	from := end.Add(-time.Duration(3600+rand.IntN(20*24*3600)) * time.Second)
	f := "2006-01-02T15:04:05"
	loc, _ := time.LoadLocation("Asia/Kolkata")
	return "/orders/count?from=" + from.In(loc).Format(f) + "&to=" + end.In(loc).Format(f)
}

func timed(url, key string, uncached bool) (time.Duration, int) {
	req := newGet(url, key)
	if uncached {
		req.Header.Set("X-Cuboid-Bench", "uncached")
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0
	}
	drain(resp)
	return time.Since(t0), resp.StatusCode
}

func runSpeed(w *world, viewers int, quick bool) {
	rounds := 5
	stores := w.stores
	if quick {
		rounds = 2
		if len(stores) > 10 {
			stores = stores[:10]
		}
	}
	art := speedArtifact{Tier: w.tier, GeneratedAt: time.Now(), Environment: environment(), Stores: len(w.stores)}
	for _, q := range speedQueries {
		var un, ca, lg []float64
		worst := map[int32]float64{}
		for r := 0; r < rounds; r++ {
			for i, s := range stores {
				path := q.Path
				if path == "" {
					path = arbitrary()
				}
				url := fmt.Sprintf("%s/tenants/%d%s", cuboidURL, s.ID, path)
				d, code := timed(url, w.op, true)
				if code != 200 {
					fatal(fmt.Errorf("%s: status %d", url, code))
				}
				v := float64(d.Microseconds()) / 1000
				un = append(un, v)
				if v > worst[s.ID] {
					worst[s.ID] = v
				}
				if i == 0 {
					lg = append(lg, v)
				}
				timed(url, w.op, false) // fills the cache
				d, _ = timed(url, w.op, false)
				ca = append(ca, float64(d.Microseconds())/1000)
			}
		}
		var ws int32
		var wv float64
		for k, v := range worst {
			if v > wv {
				ws, wv = k, v
			}
		}
		row := speedRow{Query: q.Name, Uncached: monitor.Summarise("uncached", un), Cached: monitor.Summarise("cached", ca), WorstStore: ws, Largest: monitor.Summarise("largest", lg)}
		art.Rows = append(art.Rows, row)
		fmt.Printf("  %-38s uncached p50 %7.1f p99 %7.1f ms   cached p50 %6.1f p99 %6.1f ms   worst store %d\n", q.Name, row.Uncached.P50, row.Uncached.P99, row.Cached.P50, row.Cached.P99, ws)
	}
	// 1,000 viewers through Cuboid, as the dashboards see it.
	if _, err := postJSON(loadgenURL+"/control", "", map[string]any{"mode": "cuboid", "viewers": viewers}, nil); err == nil {
		time.Sleep(15 * time.Second)
		var st struct {
			P50    float64 `json:"p50_ms"`
			P99    float64 `json:"p99_ms"`
			Errors int64   `json:"errors"`
		}
		var p99s, p50s []float64
		for i := 0; i < 6; i++ {
			time.Sleep(5 * time.Second)
			if getJSON(loadgenURL+"/state", "", &st) == nil {
				p99s, p50s = append(p99s, st.P99), append(p50s, st.P50)
			}
		}
		art.Viewers, art.ViewersErr = viewers, st.Errors
		art.ViewersP99 = monitor.Summarise("v", p99s).P50
		art.ViewersP50 = monitor.Summarise("v", p50s).P50
		postJSON(loadgenURL+"/control", "", map[string]any{"mode": "off"}, nil)
		fmt.Printf("  %d viewers through Cuboid: p50 %.1f ms, p99 %.1f ms, %d errors\n", viewers, art.ViewersP50, art.ViewersP99, art.ViewersErr)
	}
	writeArtifact("speed", w.tier, art)
}

type freshArtifact struct {
	Tier        string              `json:"tier"`
	GeneratedAt time.Time           `json:"generated_at"`
	Inserts     monitor.Percentiles `json:"insert_to_visible_ms"`
	Updates     monitor.Percentiles `json:"update_to_visible_ms"`
	TimedOut    int                 `json:"timed_out"`
}

// runFreshness times probe orders through the store app until they are visible
// through the API. Each probe uses its own reserved minute long before the
// data starts, so it is the only order in the range; each is deleted afterwards.
func runFreshness(w *world, quick bool) {
	n := 200
	if quick {
		n = 30
	}
	store := w.stores[0]
	base := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rand.IntN(100000)) * time.Hour)
	var mu sync.Mutex
	var ins, upd []float64
	timedOut := 0
	var ids []int64
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	visible := func(q string, want string) bool {
		end := time.Now().Add(60 * time.Second)
		for time.Now().Before(end) {
			var r struct {
				Data struct {
					Items []struct {
						Key   string `json:"key"`
						Value int64  `json:"value"`
					} `json:"items"`
				} `json:"data"`
			}
			if getJSON(q, w.op, &r) == nil {
				for _, it := range r.Data.Items {
					if it.Key == want && it.Value == 1 {
						return true
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		return false
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			at := base.Add(time.Duration(i) * time.Minute)
			q := fmt.Sprintf("%s/tenants/%d/orders/breakdown?by=status&from=%s&to=%s", cuboidURL, store.ID, at.Format(time.RFC3339), at.Add(time.Minute).Format(time.RFC3339))
			var o struct {
				OrderID int64 `json:"order_id"`
			}
			t0 := time.Now()
			code, err := postJSON(fmt.Sprintf("%s/app/tenants/%d/orders", appURL, store.ID), "", map[string]any{"created_at": at.Add(30 * time.Second).Format(time.RFC3339)}, &o)
			if err != nil || code != 201 {
				fatal(fmt.Errorf("probe insert: %v status %d", err, code))
			}
			okIns := visible(q, "placed")
			d1 := time.Since(t0)
			t1 := time.Now()
			patch(fmt.Sprintf("%s/app/tenants/%d/orders/%d", appURL, store.ID, o.OrderID), `{"status":"cancelled"}`)
			okUpd := visible(q, "cancelled")
			d2 := time.Since(t1)
			mu.Lock()
			if okIns {
				ins = append(ins, float64(d1.Milliseconds()))
			} else {
				timedOut++
			}
			if okUpd {
				upd = append(upd, float64(d2.Milliseconds()))
			} else {
				timedOut++
			}
			ids = append(ids, o.OrderID)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		del(fmt.Sprintf("%s/app/tenants/%d/orders/%d", appURL, store.ID, id))
	}
	art := freshArtifact{Tier: w.tier, GeneratedAt: time.Now(), Inserts: monitor.Summarise("insert", ins), Updates: monitor.Summarise("update", upd), TimedOut: timedOut}
	fmt.Printf("  insert to visible p50 %.0f p99 %.0f max %.0f ms; update to visible p50 %.0f p99 %.0f max %.0f ms; %d timed out\n",
		art.Inserts.P50, art.Inserts.P99, art.Inserts.Max, art.Updates.P50, art.Updates.P99, art.Updates.Max, timedOut)
	writeArtifact("freshness", w.tier, art)
}

type safetyPhase struct {
	Name       string  `json:"name"`
	P50        float64 `json:"app_p50_ms"`
	P95        float64 `json:"app_p95_ms"`
	P99        float64 `json:"app_p99_ms"`
	Ops        float64 `json:"app_ops_per_s"`
	SlowerP50  float64 `json:"slower_p50_pct"`
	SlowerP95  float64 `json:"slower_p95_pct"`
	Viewers    int     `json:"viewers"`
	DashP99    float64 `json:"dashboard_p99_ms"`
	DashErrors int64   `json:"dashboard_errors"`
}

type safetyArtifact struct {
	Tier        string            `json:"tier"`
	GeneratedAt time.Time         `json:"generated_at"`
	Environment map[string]string `json:"environment"`
	Phases      []safetyPhase     `json:"phases"`
	Note        string            `json:"note"`
}

// measurePhase reads the app's 30-second windows and takes the median.
func measurePhase(name string, phase time.Duration, viewers int) safetyPhase {
	time.Sleep(15 * time.Second) // warm-up
	var p50, p95, p99, ops, dash []float64
	var dashErr int64
	end := time.Now().Add(phase)
	for time.Now().Before(end) {
		time.Sleep(10 * time.Second)
		var m struct {
			P50 float64 `json:"p50_ms"`
			P95 float64 `json:"p95_ms"`
			P99 float64 `json:"p99_ms"`
			Ops float64 `json:"ops_per_s"`
		}
		if getJSON(appURL+"/metrics/window?seconds=10", "", &m) == nil {
			p50, p95, p99, ops = append(p50, m.P50), append(p95, m.P95), append(p99, m.P99), append(ops, m.Ops)
		}
		var st struct {
			P99    float64 `json:"p99_ms"`
			Errors int64   `json:"errors"`
		}
		if viewers > 0 && getJSON(loadgenURL+"/state", "", &st) == nil {
			dash, dashErr = append(dash, st.P99), st.Errors
		}
	}
	med := func(v []float64) float64 { return monitor.Summarise("", v).P50 }
	p := safetyPhase{Name: name, P50: med(p50), P95: med(p95), P99: med(p99), Ops: med(ops), Viewers: viewers, DashP99: med(dash), DashErrors: dashErr}
	fmt.Printf("  %-34s app p50 %6.2f p95 %6.2f p99 %6.2f ms, %.0f ops/s, dashboards p99 %.1f ms\n", name, p.P50, p.P95, p.P99, p.Ops, p.DashP99)
	return p
}

// settle waits for Cuboid to catch up after a restart. The slowdown while it
// catches up is real and documented, but it is not the steady cost being measured.
func settle() {
	for i := 0; i < 600; i++ {
		var s struct {
			Freshness struct {
				LagMS int64 `json:"lag_ms"`
			} `json:"freshness"`
		}
		if getJSON(cuboidURL+"/api/monitor/state?since=9999999999999", "", &s) == nil && s.Freshness.LagMS >= 0 && s.Freshness.LagMS < 2000 {
			break
		}
		time.Sleep(time.Second)
	}
	time.Sleep(60 * time.Second)
}

func runSafety(w *world, phase time.Duration, viewers int) {
	art := safetyArtifact{Tier: w.tier, GeneratedAt: time.Now(), Environment: environment(),
		Note: "Same data, same app workload, same machine; databases, Cuboid, the app and the load generator share the machine's cores."}
	postJSON(loadgenURL+"/control", "", map[string]any{"mode": "off"}, nil)
	fmt.Println("Stopping Cuboid for the app-alone baseline:", stopCmd)
	if err := shell(stopCmd); err != nil {
		fatal(err)
	}
	alone := measurePhase("App alone (no Cuboid, no dashboards)", phase, 0)
	if err := shell(startCmd); err != nil {
		fatal(err)
	}
	if err := waitHealthy(5 * time.Minute); err != nil {
		fatal(err)
	}
	settle()
	var phases []safetyPhase
	phases = append(phases, alone, measurePhase("Cuboid running, no dashboards", phase, 0))
	postJSON(loadgenURL+"/control", "", map[string]any{"mode": "cuboid", "viewers": viewers}, nil)
	phases = append(phases, measurePhase(fmt.Sprintf("Cuboid, %d dashboards", viewers), phase, viewers))
	postJSON(loadgenURL+"/control", "", map[string]any{"mode": "direct", "viewers": viewers}, nil)
	phases = append(phases, measurePhase(fmt.Sprintf("Direct, %d dashboards", viewers), phase, viewers))
	postJSON(loadgenURL+"/control", "", map[string]any{"mode": "off"}, nil)
	for i := range phases {
		if alone.P95 > 0 && alone.P50 > 0 {
			phases[i].SlowerP95 = 100 * (phases[i].P95 - alone.P95) / alone.P95
			phases[i].SlowerP50 = 100 * (phases[i].P50 - alone.P50) / alone.P50
		}
	}
	art.Phases = phases
	for _, p := range phases {
		fmt.Printf("  %-34s p95 %+.1f%%, p50 %+.1f%% against app alone\n", p.Name, p.SlowerP95, p.SlowerP50)
	}
	writeArtifact("safety", w.tier, art)
}

type correctnessArtifact struct {
	Tier        string    `json:"tier"`
	GeneratedAt time.Time `json:"generated_at"`
	Quiesced    batchOut  `json:"quiesced"`
	Live        batchOut  `json:"live"`
}

type batchOut struct {
	Total    int      `json:"total"`
	Exact    int      `json:"exact"`
	Mismatch int      `json:"mismatch"`
	Failed   int      `json:"failed"`
	Errors   []string `json:"errors"`
	Seconds  float64  `json:"seconds"`
}

func (w *world) batch(n int) batchOut {
	// Someone may have pressed Verify everything: wait for that batch rather
	// than abandon the run, which would also leave the store app paused.
	var code int
	var err error
	for i := 0; i < 600; i++ {
		code, err = postJSON(fmt.Sprintf("%s/admin/verify-batch?n=%d", cuboidURL, n), w.op, map[string]any{}, nil)
		if err != nil || code != 409 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil || code != 202 {
		postJSON(appURL+"/control/rates", "", map[string]any{"paused": false}, nil)
		fatal(fmt.Errorf("start batch: %v status %d", err, code))
	}
	t0 := time.Now()
	for {
		time.Sleep(2 * time.Second)
		var s struct {
			Verify struct {
				Batch struct {
					Running  bool     `json:"running"`
					Total    int      `json:"total"`
					Done     int      `json:"done"`
					Exact    int      `json:"exact"`
					Mismatch int      `json:"mismatch"`
					Failed   int      `json:"failed"`
					Errors   []string `json:"errors"`
				} `json:"batch"`
			} `json:"verify"`
		}
		if err := getJSON(cuboidURL+"/api/monitor/state?since=9999999999999", "", &s); err != nil {
			continue
		}
		b := s.Verify.Batch
		if !b.Running {
			return batchOut{Total: b.Total, Exact: b.Exact, Mismatch: b.Mismatch, Failed: b.Failed, Errors: b.Errors, Seconds: time.Since(t0).Seconds()}
		}
	}
}

func runCorrectness(w *world, quick bool) {
	nq, nl := 2000, 500
	if quick {
		nq, nl = 100, 50
	}
	art := correctnessArtifact{Tier: w.tier, GeneratedAt: time.Now()}
	postJSON(appURL+"/control/rates", "", map[string]any{"paused": true}, nil)
	time.Sleep(3 * time.Second)
	art.Quiesced = w.batch(nq)
	postJSON(appURL+"/control/rates", "", map[string]any{"paused": false}, nil)
	fmt.Printf("  quiesced: %d of %d exact, %d mismatched, %d did not complete (%.0f s)\n", art.Quiesced.Exact, art.Quiesced.Total, art.Quiesced.Mismatch, art.Quiesced.Failed, art.Quiesced.Seconds)
	art.Live = w.batch(nl)
	fmt.Printf("  while writing: %d of %d exact, %d mismatched, %d did not complete (%.0f s)\n", art.Live.Exact, art.Live.Total, art.Live.Mismatch, art.Live.Failed, art.Live.Seconds)
	writeArtifact("correctness", w.tier, art)
}

type growthRow struct {
	Query        string  `json:"query"`
	UncachedP50  float64 `json:"uncached_p50_ratio"`
	UncachedP99  float64 `json:"uncached_p99_ratio"`
	CachedP50    float64 `json:"cached_p50_ratio"`
	CachedP99    float64 `json:"cached_p99_ratio"`
	SmallP99MS   float64 `json:"small_uncached_p99_ms"`
	MainP99MS    float64 `json:"main_uncached_p99_ms"`
	AbsDiffP99MS float64 `json:"absolute_difference_p99_ms"`
}

type growthArtifact struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Rows        []growthRow `json:"rows"`
	WorstP99    float64     `json:"worst_uncached_p99_ratio"`
	Note        string      `json:"note"`
}

func readJSON(kind, name string, v any) error {
	b, err := os.ReadFile(filepath.Join(artifacts, kind, name+".json"))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func runGrowth() {
	var small, big speedArtifact
	if err := readJSON("speed", "small", &small); err != nil {
		fmt.Println("  growth: no small-tier speed artifact yet; seed small and run bench speed first")
		return
	}
	if err := readJSON("speed", "main", &big); err != nil {
		fmt.Println("  growth: no main-tier speed artifact yet; seed main and run bench speed first")
		return
	}
	art := growthArtifact{GeneratedAt: time.Now(), Note: "Ratios of values under 10 ms are mostly noise; the absolute difference is published beside each ratio."}
	ratio := func(a, b float64) float64 {
		if b <= 0 {
			return 0
		}
		return a / b
	}
	for i, r := range big.Rows {
		if i >= len(small.Rows) {
			break
		}
		s := small.Rows[i]
		g := growthRow{Query: r.Query, UncachedP50: ratio(r.Uncached.P50, s.Uncached.P50), UncachedP99: ratio(r.Uncached.P99, s.Uncached.P99),
			CachedP50: ratio(r.Cached.P50, s.Cached.P50), CachedP99: ratio(r.Cached.P99, s.Cached.P99),
			SmallP99MS: s.Uncached.P99, MainP99MS: r.Uncached.P99, AbsDiffP99MS: r.Uncached.P99 - s.Uncached.P99}
		if g.UncachedP99 > art.WorstP99 {
			art.WorstP99 = g.UncachedP99
		}
		art.Rows = append(art.Rows, g)
		fmt.Printf("  %-38s p99 %6.1f to %6.1f ms = %.2fx (p50 %.2fx; cached p99 %.2fx)\n", r.Query, s.Uncached.P99, r.Uncached.P99, g.UncachedP99, g.UncachedP50, g.CachedP99)
	}
	writeArtifact("growth", "latest", art)
}

// ScoreRow is one row of the PRD's targets table with its measured value.
type ScoreRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	How      string `json:"how"`
	Good     string `json:"good"`
	Great    string `json:"great"`
	Measured string `json:"measured"`
	Verdict  string `json:"verdict"`
	Command  string `json:"command"`
}

func verdict(v, good, great float64) string {
	switch {
	case v < great:
		return "Great"
	case v < good:
		return "Good"
	}
	return "Not met"
}

// writeScorecard turns the artifacts into the PRD's table. A row without an
// artifact says so; nothing is filled in.
func writeScorecard(w *world) {
	tier := w.tier
	rows := []ScoreRow{}
	missing := func(id, name, how, good, great, cmd string) ScoreRow {
		return ScoreRow{ID: id, Name: name, How: how, Good: good, Great: great, Measured: "Not measured", Verdict: "Not measured", Command: cmd}
	}
	{
		r := missing("T1", "Query speed", "Time for each dashboard query at "+tier+" tier", "Under 1 second", "Under 200 ms", "make speed-test")
		var a speedArtifact
		if readJSON("speed", tier, &a) == nil {
			// The largest store is judged on its own: pooled across 100 stores it
			// is 1% of the samples, and a p99 would hide it.
			worst := 0.0
			for _, q := range a.Rows {
				worst = max(worst, q.Uncached.P99, q.Largest.P99)
			}
			top := worst
			if a.ViewersP99 > top {
				top = a.ViewersP99
			}
			r.Measured = fmt.Sprintf("Worst query p99 %.0f ms uncached (any store, largest included); p99 %.0f ms with %d dashboards", worst, a.ViewersP99, a.Viewers)
			r.Verdict = verdict(top, 1000, 200)
		}
		rows = append(rows, r)
	}
	{
		r := missing("T2", "Freshness", "Time from adding an order to seeing it in the numbers", "Under 5 minutes", "Under 30 seconds", "make freshness-test")
		var a freshArtifact
		if readJSON("freshness", tier, &a) == nil {
			worst := a.Inserts.P99
			if a.Updates.P99 > worst {
				worst = a.Updates.P99
			}
			r.Measured = fmt.Sprintf("p99 %.1f s (insert %.1f s, update %.1f s)", worst/1000, a.Inserts.P99/1000, a.Updates.P99/1000)
			r.Verdict = verdict(worst/1000, 300, 30)
			if a.TimedOut > 0 {
				r.Verdict = "Not met"
				r.Measured += fmt.Sprintf("; %d timed out", a.TimedOut)
			}
		}
		rows = append(rows, r)
	}
	{
		r := missing("T3", "Database safety", "Normal app query time, with and without dashboard load", "Less than 25% slower", "Less than 10% slower", "make safety-test")
		var a safetyArtifact
		if readJSON("safety", tier, &a) == nil && len(a.Phases) >= 3 {
			c := a.Phases[2]
			r.Measured = fmt.Sprintf("p95 %+.1f%% (%.2f to %.2f ms) with %d dashboards through Cuboid", c.SlowerP95, a.Phases[0].P95, c.P95, c.Viewers)
			r.Verdict = verdict(c.SlowerP95, 25, 10)
		}
		rows = append(rows, r)
	}
	{
		r := missing("T4", "Correctness", "Compare your numbers with a direct database query", "Matches on most queries", "Matches on all queries", "make correctness-test")
		var a correctnessArtifact
		if readJSON("correctness", tier, &a) == nil {
			exact := a.Quiesced.Exact + a.Live.Exact
			total := a.Quiesced.Total + a.Live.Total
			r.Measured = fmt.Sprintf("%d of %d exact (%d while writes ran)", exact, total, a.Live.Exact)
			switch {
			case total > 0 && exact == total:
				r.Verdict = "Great"
			case total > 0 && float64(exact)/float64(total) >= 0.9:
				r.Verdict = "Good"
			default:
				r.Verdict = "Not met"
			}
		}
		rows = append(rows, r)
	}
	{
		r := missing("T5", "Growth", "Query speed at 1M vs 10M records", "Less than 5x slower", "Less than 2x slower", "make growth-test")
		var a growthArtifact
		if readJSON("growth", "latest", &a) == nil && len(a.Rows) > 0 {
			r.Measured = fmt.Sprintf("Worst uncached p99 ratio %.2fx", a.WorstP99)
			r.Verdict = verdict(a.WorstP99, 5, 2)
		}
		rows = append(rows, r)
	}
	card := map[string]any{"available": true, "generated_at": time.Now().Format("2006-01-02 15:04"), "tier": tier, "environment": environment(), "rows": rows}
	writeArtifact("targets", "", card)
	var b strings.Builder
	fmt.Fprintf(&b, "# Targets scorecard\n\nGenerated by `bench scorecard` from the artifacts in `artifacts/`, on the %s tier, %s. Every service shares one machine (%s logical CPUs).\n\n", tier, time.Now().Format("2 January 2006 15:04"), environment()["cpus"])
	fmt.Fprintln(&b, "| What we measure | How | Good | Great | Measured | Verdict | Reproduce |\n|---|---|---|---|---|---|---|")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | `%s` |\n", r.Name, r.How, r.Good, r.Great, r.Measured, r.Verdict, r.Command)
	}
	os.MkdirAll("docs", 0o755)
	if err := os.WriteFile("docs/TARGETS.md", []byte(b.String()), 0o644); err != nil {
		fatal(err)
	}
	for _, r := range rows {
		fmt.Printf("  %-16s %-10s %s\n", r.Name, r.Verdict, r.Measured)
	}
}
