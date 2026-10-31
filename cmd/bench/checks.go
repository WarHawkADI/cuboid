package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cuboid/internal/monitor"
)

func newGet(url, key string) *http.Request {
	req, _ := http.NewRequest("GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func patch(url, body string) int {
	req, _ := http.NewRequest("PATCH", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	drain(resp)
	return resp.StatusCode
}

func del(url string) int {
	req, _ := http.NewRequest("DELETE", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	drain(resp)
	return resp.StatusCode
}

type checkRow struct {
	ID     string `json:"id"`
	Check  string `json:"check"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// runDemoCheck is the golden acceptance table: the PRD's examples, verbatim.
func runDemoCheck(ctx context.Context, w *world) bool {
	var rows []checkRow
	add := func(id, check string, pass bool, detail string) {
		rows = append(rows, checkRow{id, check, pass, detail})
		mark := "pass"
		if !pass {
			mark = "FAIL"
		}
		fmt.Printf("  %-4s %-4s %-58s %s\n", id, mark, check, detail)
	}
	big, small := w.stores[0], w.stores[len(w.stores)-1]
	verify := func(id, what, scope, ep string, p map[string]string) {
		ok, det, err := w.check(scope, ep, p)
		if err != nil {
			add(id, what, false, err.Error())
			return
		}
		add(id, what, ok, det)
	}
	s := func(st demoKey) string { return fmt.Sprintf("/tenants/%d", st.ID) }
	verify("D1", "Orders and total sales, last 7 days, largest store", s(big), "orders/summary", map[string]string{"range": "last_7d"})
	verify("D1", "Orders and total sales, last 7 days, smallest store", s(small), "orders/summary", map[string]string{"range": "last_7d"})
	verify("D2", "Orders per day, last 30 days", s(big), "orders/trend", map[string]string{"interval": "day", "range": "last_30d"})
	verify("D3", "Top 10 products by sales this week", s(big), "products/top", map[string]string{"n": "10", "by": "sales", "range": "this_week"})
	verify("D4", "Orders by status", s(big), "orders/breakdown", map[string]string{"by": "status", "range": "last_7d"})

	// FR-6: every kind of change, in a reserved minute only the probe uses.
	at := time.Date(2002, 3, 4, 5, 0, 0, 0, time.UTC).Add(time.Duration(rand.IntN(50000)) * time.Hour)
	statusQ := fmt.Sprintf("%s/tenants/%d/orders/breakdown?by=status&from=%s&to=%s", cuboidURL, big.ID, at.Format(time.RFC3339), at.Add(time.Minute).Format(time.RFC3339))
	sumQ := fmt.Sprintf("%s/tenants/%d/orders/summary?from=%s&to=%s", cuboidURL, big.ID, at.Format(time.RFC3339), at.Add(time.Minute).Format(time.RFC3339))
	until := func(q string, cond func(string) bool) (time.Duration, bool) {
		t0 := time.Now()
		for time.Since(t0) < 20*time.Second {
			resp, err := client.Do(newGet(q, w.op))
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if cond(string(b)) {
					return time.Since(t0), true
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		return time.Since(t0), false
	}
	var o struct {
		OrderID     int64 `json:"order_id"`
		AmountPaise int64 `json:"amount_paise"`
	}
	postJSON(fmt.Sprintf("%s/app/tenants/%d/orders", appURL, big.ID), "", map[string]any{"created_at": at.Add(20 * time.Second).Format(time.RFC3339)}, &o)
	d, ok := until(statusQ, func(b string) bool { return strings.Contains(b, `"key":"placed","label":"placed","value":1`) })
	add("D5", "Add an order: the count goes up", ok && d <= 5*time.Second, d.Round(time.Millisecond).String())
	orderURL := fmt.Sprintf("%s/app/tenants/%d/orders/%d", appURL, big.ID, o.OrderID)
	patch(orderURL, `{"status":"cancelled"}`)
	d, ok = until(statusQ, func(b string) bool {
		return strings.Contains(b, `"key":"cancelled"`) && !strings.Contains(b, `"key":"placed"`)
	})
	add("D6", "Order moves from placed to cancelled", ok && d <= 5*time.Second, d.Round(time.Millisecond).String())
	patch(orderURL, `{"amount_paise":123456}`)
	d, ok = until(sumQ, func(b string) bool { return strings.Contains(b, `"sales":"1234.56"`) })
	add("D7", "Amount corrected", ok && d <= 5*time.Second, d.Round(time.Millisecond).String())
	del(orderURL)
	d, ok = until(sumQ, func(b string) bool { return strings.Contains(b, `"orders":0`) })
	add("D7", "Order deleted", ok && d <= 5*time.Second, d.Round(time.Millisecond).String())
	back := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	var o2 struct {
		OrderID int64 `json:"order_id"`
	}
	postJSON(fmt.Sprintf("%s/app/tenants/%d/orders", appURL, big.ID), "", map[string]any{"created_at": back.Format(time.RFC3339)}, &o2)
	time.Sleep(2500 * time.Millisecond)
	verify("D7", "Backdated order inserted, then checked", s(big), "orders/count", map[string]string{"from": back.Add(-time.Hour).Format(time.RFC3339), "to": back.Add(time.Hour).Format(time.RFC3339)})
	del(fmt.Sprintf("%s/app/tenants/%d/orders/%d", appURL, big.ID, o2.OrderID))
	verify("D8", "count?from=...&to=... with arbitrary timestamps", s(big), "orders/count", map[string]string{"from": time.Now().Add(-97*time.Hour - 13*time.Second).Format(time.RFC3339), "to": time.Now().Add(-3*time.Hour - 41*time.Second).Format(time.RFC3339)})

	// FR-5: one store's key on every endpoint of another store.
	other := w.stores[1]
	allNotFound := true
	var bad []string
	for _, ep := range []string{"orders/count", "orders/summary", "orders/trend", "products/top", "customers/top", "cities/top", "orders/breakdown?by=status", "meta", "orders/stream?query=orders/count"} {
		url := fmt.Sprintf("%s/tenants/%d/%s", cuboidURL, other.ID, ep)
		resp, err := client.Do(newGet(url, big.Key))
		if err != nil || resp.StatusCode != 404 {
			allNotFound = false
			bad = append(bad, ep)
		}
		if err == nil {
			drain(resp)
		}
	}
	add("D9", fmt.Sprintf("Store %d's key on store %d, every endpoint", big.ID, other.ID), allNotFound, strings.Join(bad, ", "))

	if len(w.companies) > 0 {
		var co demoKey
		for _, c := range w.companies {
			if len(c.Stores) > len(co.Stores) {
				co = c
			}
		}
		verify("D10", fmt.Sprintf("Company %d, all %d stores", co.ID, len(co.Stores)), fmt.Sprintf("/companies/%d", co.ID), "orders/summary", map[string]string{"range": "last_30d"})
		var outside int32
		for _, st := range w.stores {
			inside := false
			for _, x := range co.Stores {
				inside = inside || x == st.ID
			}
			if !inside {
				outside = st.ID
				break
			}
		}
		resp, err := client.Do(newGet(fmt.Sprintf("%s/tenants/%d/orders/count", cuboidURL, outside), co.Key))
		add("D10", "Company key on a store of another company", err == nil && resp.StatusCode == 404, "")
		if err == nil {
			drain(resp)
		}
	}

	var env struct {
		Freshness struct {
			LSN     string `json:"lsn"`
			LagMS   int64  `json:"lag_ms"`
			Updated string `json:"updated"`
			AsOf    string `json:"as_of"`
		} `json:"freshness"`
	}
	err := getJSON(fmt.Sprintf("%s/tenants/%d/orders/count?range=today", cuboidURL, big.ID), big.Key, &env)
	add("D11", "Response states as_of, LSN, lag and updated", err == nil && env.Freshness.AsOf != "" && env.Freshness.LSN != "" && env.Freshness.LagMS < 5000 && env.Freshness.Updated != "", env.Freshness.Updated)

	rebuild := func(source string) {
		code, _ := postJSON(fmt.Sprintf("%s/admin/rebuild?source=%s&scope=all", cuboidURL, source), w.op, nil, nil)
		time.Sleep(time.Second)
		failed, answered := 0, 0
		for {
			var st struct {
				Rebuild struct {
					Running bool   `json:"running"`
					Error   string `json:"error"`
				} `json:"rebuild"`
				State string `json:"state"`
			}
			getJSON(cuboidURL+"/api/monitor/state?since=9999999999999", "", &st)
			resp, err := client.Do(newGet(fmt.Sprintf("%s/tenants/%d/orders/count?range=last_30d", cuboidURL, big.ID), big.Key))
			if err != nil || resp.StatusCode != 200 {
				failed++
			} else {
				answered++
			}
			if err == nil {
				drain(resp)
			}
			if !st.Rebuild.Running && st.State == "streaming" {
				if st.Rebuild.Error != "" {
					add("D12", "Rebuild from "+source, false, st.Rebuild.Error)
					return
				}
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		ok, det, err := w.check(s(big), "orders/breakdown", map[string]string{"by": "category", "range": "last_90d"})
		add("D12", "Rebuild from "+source+" while queries answer", code == 202 && failed == 0 && err == nil && ok, fmt.Sprintf("%d answered, %d failed during it; afterwards %s", answered, failed, det))
	}
	rebuild("mirror")
	rebuild("main")

	var flood struct {
		HeavyLimited int     `json:"heavy_limited"`
		LightLimited int     `json:"light_limited"`
		LightOK      int     `json:"own_light_answered"`
		Before       float64 `json:"others_p99_before_ms"`
		During       float64 `json:"others_p99_during_ms"`
		OthersFailed int     `json:"others_failed"`
	}
	postJSON(cuboidURL+"/admin/flood", w.op, map[string]any{"store": big.ID, "seconds": 8}, &flood)
	limited := flood.HeavyLimited + flood.LightLimited
	add("D13", "Biggest store floods: it is limited, others are not", limited > 0 && flood.OthersFailed == 0, fmt.Sprintf("%d limited (%d heavy), others' p99 %.1f to %.1f ms, %d of its own light queries answered", limited, flood.HeavyLimited, flood.Before, flood.During, flood.LightOK))

	postJSON(cuboidURL+"/admin/demo/metric", w.op, map[string]any{"add": false}, nil)
	var rep struct {
		OK     bool   `json:"ok"`
		TookMS int64  `json:"took_ms"`
		Error  string `json:"error"`
	}
	postJSON(cuboidURL+"/admin/demo/metric", w.op, map[string]any{"add": true}, &rep)
	ok, det, verr := w.check(s(big), "orders/breakdown", map[string]string{"by": "payment_method", "range": "last_7d"})
	add("D14", "Add status_payment in YAML: breakdown by payment method", rep.OK && verr == nil && ok, fmt.Sprintf("live in %d ms; %s%s", rep.TookMS, det, rep.Error))
	postJSON(cuboidURL+"/admin/demo/metric", w.op, map[string]any{"add": false}, nil)

	var mon struct {
		Latency   []any `json:"latency"`
		Freshness struct {
			LagMS int64 `json:"lag_ms"`
		} `json:"freshness"`
		Recorder struct {
			Points []any `json:"points"`
		} `json:"recorder"`
	}
	merr := getJSON(cuboidURL+"/api/monitor/state", "", &mon)
	add("D15", "Monitoring state: query time, data lag, recorder", merr == nil && len(mon.Latency) > 0 && len(mon.Recorder.Points) > 0, fmt.Sprintf("lag %d ms, %d endpoints, %d points (the browser check is separate)", mon.Freshness.LagMS, len(mon.Latency), len(mon.Recorder.Points)))

	for _, m := range []string{"A", "B", "C"} {
		var d struct {
			Injected int64 `json:"faults_injected"`
			Blocked  int64 `json:"blocked"`
			Leaked   int   `json:"leaked"`
			Requests int   `json:"requests"`
		}
		postJSON(cuboidURL+"/admin/drill", w.op, map[string]any{"mode": m, "seconds": 5}, &d)
		add("D17", "Leak drill "+m, d.Leaked == 0 && d.Injected > 0 && d.Blocked > 0, fmt.Sprintf("%d requests, %d faults, %d blocked, %d leaked", d.Requests, d.Injected, d.Blocked, d.Leaked))
	}
	writeArtifact("demo-check", w.tier, map[string]any{"generated_at": time.Now(), "tier": w.tier, "rows": rows})
	pass := 0
	for _, r := range rows {
		if r.Pass {
			pass++
		}
	}
	fmt.Printf("%d of %d checks pass\n", pass, len(rows))
	return pass == len(rows)
}

// runIsolation is the isolation suite: randomized requests across every store
// and company, concurrently, with random client cancellations (the INC-02
// trigger), each answer compared with what the requester should see.
func runIsolation(ctx context.Context, w *world, total int) bool {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	r := "from=" + m.AddDate(0, -4, 0).Format("2006-01-02") + "&to=" + m.AddDate(0, -2, 0).Format("2006-01-02")
	queries := []string{"/orders/count?" + r, "/orders/summary?" + r, "/orders/breakdown?by=status&" + r, "/products/top?n=10&" + r, "/orders/trend?interval=month&" + r, "/orders/breakdown?by=category&" + r}
	type principal struct {
		path string
		key  string
		id   int32
	}
	var ps []principal
	for _, s := range w.stores {
		ps = append(ps, principal{fmt.Sprintf("/tenants/%d", s.ID), s.Key, s.ID})
	}
	for _, c := range w.companies {
		ps = append(ps, principal{fmt.Sprintf("/companies/%d", c.ID), c.Key, -c.ID})
	}
	// The store app is paused so past months cannot change under the test.
	postJSON(appURL+"/control/rates", "", map[string]any{"paused": true}, nil)
	defer postJSON(appURL+"/control/rates", "", map[string]any{"paused": false}, nil)
	time.Sleep(3 * time.Second)
	expected := map[string]string{}
	for _, p := range ps {
		for _, q := range queries {
			var body struct {
				Data json.RawMessage `json:"data"`
			}
			if err := getJSON(cuboidURL+p.path+q, w.op, &body); err != nil {
				fatal(err)
			}
			expected[p.path+q] = string(body.Data)
		}
	}
	// A valid out-of-scope answer and a non-existent id must be identical.
	nf := func(path, key string) (int, string) {
		resp, err := client.Do(newGet(cuboidURL+path, key))
		if err != nil {
			return 0, ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	c1, b1 := nf(fmt.Sprintf("/tenants/%d/orders/count", w.stores[1].ID), w.stores[0].Key)
	c2, b2 := nf("/tenants/999999/orders/count", w.stores[0].Key)
	sameNotFound := c1 == 404 && c2 == 404 && b1 == b2

	var sent, okCount, wrong, crossed, cancelled, limited, other atomic.Int64
	var mu sync.Mutex
	var lat []float64
	workers := 64
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for int(sent.Add(1)) <= total {
				p := ps[rand.IntN(len(ps))]
				q := queries[rand.IntN(len(queries))]
				rctx, cancel := context.WithCancel(ctx)
				req := newGet(cuboidURL+p.path+q, p.key).WithContext(rctx)
				if rand.IntN(10) == 0 {
					go func() { time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond); cancel() }()
				}
				t0 := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					cancelled.Add(1)
					cancel()
					continue
				}
				b, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				cancel()
				if rerr != nil {
					cancelled.Add(1)
					continue
				}
				switch resp.StatusCode {
				case 200:
					var body struct {
						Data json.RawMessage `json:"data"`
					}
					json.Unmarshal(b, &body)
					if string(body.Data) == expected[p.path+q] {
						okCount.Add(1)
					} else {
						wrong.Add(1)
						for _, x := range ps {
							if x.path != p.path && string(body.Data) == expected[x.path+q] && expected[x.path+q] != expected[p.path+q] {
								crossed.Add(1)
								break
							}
						}
					}
				case 429:
					limited.Add(1)
				default:
					other.Add(1)
				}
				if s := sent.Load(); s%50 == 0 {
					mu.Lock()
					lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	pct := monitor.Summarise("isolation", lat)
	fmt.Printf("  %d requests in %s (%.0f/s): %d exact, %d different, %d crossed stores, %d cancelled by the client, %d limited, %d other; p50 %.1f, p99 %.1f ms\n",
		total, took.Round(time.Second), float64(total)/took.Seconds(), okCount.Load(), wrong.Load(), crossed.Load(), cancelled.Load(), limited.Load(), other.Load(), pct.P50, pct.P99)
	fmt.Printf("  out-of-scope id and non-existent id give an identical 404: %v\n", sameNotFound)
	writeArtifact("isolation", w.tier, map[string]any{"generated_at": time.Now(), "tier": w.tier, "requests": total, "exact": okCount.Load(), "different": wrong.Load(), "cross_tenant": crossed.Load(),
		"client_cancelled": cancelled.Load(), "limited": limited.Load(), "other_status": other.Load(), "identical_404": sameNotFound, "seconds": took.Seconds()})
	return crossed.Load() == 0 && wrong.Load() == 0 && other.Load() == 0 && sameNotFound
}

// runChaos kills Cuboid with SIGKILL under write load (it needs the
// supervisor) and checks exactness after every restart.
func runChaos(w *world, n int) bool {
	allOK := true
	var recov []float64
	for i := 0; i < n; i++ {
		time.Sleep(time.Duration(1000+rand.IntN(4000)) * time.Millisecond)
		code, err := postJSON(cuboidURL+"/admin/chaos", w.op, map[string]any{"name": "kill"}, nil)
		if err != nil || code != 202 {
			fatal(fmt.Errorf("kill: %v %d", err, code))
		}
		t0 := time.Now()
		time.Sleep(600 * time.Millisecond)
		if err := waitHealthy(2 * time.Minute); err != nil {
			fatal(fmt.Errorf("iteration %d: %w", i+1, err))
		}
		rec := time.Since(t0)
		recov = append(recov, float64(rec.Milliseconds()))
		b := w.batch(25)
		ok := b.Exact == b.Total && b.Total == 25
		allOK = allOK && ok
		fmt.Printf("  kill %2d: streaming again in %s; %d of %d exact, %d mismatched, %d did not complete %v\n", i+1, rec.Round(time.Millisecond), b.Exact, b.Total, b.Mismatch, b.Failed, b.Errors)
	}
	p := monitor.Summarise("recovery", recov)
	writeArtifact("chaos", w.tier, map[string]any{"generated_at": time.Now(), "tier": w.tier, "kills": n, "all_exact": allOK, "recovery_p50_ms": p.P50, "recovery_max_ms": p.Max})
	return allOK
}

type stepOut struct {
	Mode    string  `json:"mode"`
	Viewers int     `json:"viewers"`
	AppP50  float64 `json:"app_p50_ms"`
	AppP95  float64 `json:"app_p95_ms"`
	Slower  float64 `json:"slower_pct"`
	DashP99 float64 `json:"dashboard_p99_ms"`
	DashErr int64   `json:"dashboard_errors"`
	Tripped bool    `json:"tripped,omitempty"`
}

// runSteps is make bench: viewers against modes, one step at a time.
func runSteps(w *world, phase time.Duration) {
	postJSON(loadgenURL+"/control", "", map[string]any{"mode": "off"}, nil)
	base := measurePhase("Cuboid running, no dashboards", phase, 0)
	steps := []stepOut{{Mode: "cuboid-no-dashboards", AppP50: base.P50, AppP95: base.P95}}
	for _, mode := range []string{"direct", "direct-cache", "cuboid"} {
		for _, v := range []int{250, 500, 1000} {
			postJSON(loadgenURL+"/control", "", map[string]any{"mode": mode, "viewers": v}, nil)
			p := measurePhase(fmt.Sprintf("%s, %d dashboards", mode, v), phase, v)
			s := stepOut{Mode: mode, Viewers: v, AppP50: p.P50, AppP95: p.P95, DashP99: p.DashP99, DashErr: p.DashErrors}
			if base.P95 > 0 {
				s.Slower = 100 * (p.P95 - base.P95) / base.P95
			}
			s.Tripped = p.P95 > 2000
			steps = append(steps, s)
			if s.Tripped {
				break // the laptop must stay usable; this path is already past saving
			}
		}
	}
	postJSON(loadgenURL+"/control", "", map[string]any{"mode": "off"}, nil)
	writeArtifact("bench", "", map[string]any{"available": true, "generated_at": time.Now().Format("2006-01-02 15:04"), "tier": w.tier, "environment": environment(), "steps": steps})
}
