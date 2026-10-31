package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// QuickRow is one row of the scorecard's "Run now" result.
type QuickRow struct {
	ID       string `json:"id"`
	Measured string `json:"measured"`
	Verdict  string `json:"verdict"`
}

// QuickRun is the state of the short re-measurement behind "Run now". It covers
// the three targets that take about a minute. Safety and growth need a load run
// and a second data size, so they stay with make targets.
type QuickRun struct {
	Running    bool       `json:"running"`
	Phase      string     `json:"phase,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt time.Time  `json:"finished_at"`
	Rows       []QuickRow `json:"rows"`
	Error      string     `json:"error,omitempty"`
}

type quickState struct {
	mu  sync.Mutex
	run QuickRun
}

func (q *quickState) snapshot() QuickRun {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.run
	r.Rows = append([]QuickRow{}, q.run.Rows...)
	return r
}

func (q *quickState) update(f func(*QuickRun)) {
	q.mu.Lock()
	f(&q.run)
	q.mu.Unlock()
}

// quickQueries are the PRD's examples, the ones T1 judges.
var quickQueries = []string{
	"/orders/summary?range=last_7d",
	"/orders/trend?interval=day&range=last_30d",
	"/products/top?n=10&by=sales&range=this_week",
	"/orders/breakdown?by=status&range=last_7d",
	"/customers/top?n=10&by=orders&range=last_30d",
}

func (c *Core) adminTargetsRun(w http.ResponseWriter, r *http.Request) {
	c.quick.mu.Lock()
	if c.quick.run.Running {
		c.quick.mu.Unlock()
		writeJSON(w, 409, map[string]string{"status": "already running"})
		return
	}
	c.quick.run = QuickRun{Running: true, StartedAt: time.Now(), Phase: "query speed"}
	c.quick.mu.Unlock()
	go c.runQuick("http://"+r.Host, r.Header.Get("Authorization"))
	writeJSON(w, 202, map[string]string{"status": "started"})
}

func (c *Core) runQuick(base, auth string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store := c.largestStore()
	var rows []QuickRow
	row, err := c.quickSpeed(ctx, base, auth, store)
	if err == nil {
		rows = append(rows, row)
		c.quick.update(func(q *QuickRun) { q.Rows, q.Phase = rows, "freshness" })
		row, err = c.quickFreshness(ctx, base, auth, store)
	}
	if err == nil {
		rows = append(rows, row)
		c.quick.update(func(q *QuickRun) { q.Rows, q.Phase = rows, "correctness" })
		row, err = c.quickCorrectness(ctx)
	}
	if err == nil {
		rows = append(rows, row)
	}
	c.quick.update(func(q *QuickRun) {
		q.Running, q.Phase, q.Rows, q.FinishedAt = false, "", rows, time.Now()
		if err != nil {
			q.Error = err.Error()
		}
	})
	c.Rec.Annotate("Targets re-measured")
}

func quickGet(ctx context.Context, url, auth string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-Cuboid-Bench", "uncached")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	return resp.StatusCode, b, err
}

// quickSpeed takes the slowest uncached time of the examples on the largest store.
func (c *Core) quickSpeed(ctx context.Context, base, auth string, store int32) (QuickRow, error) {
	var worst time.Duration
	worstQuery := ""
	for _, q := range quickQueries {
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			code, _, err := quickGet(ctx, fmt.Sprintf("%s/tenants/%d%s", base, store, q), auth)
			if err != nil {
				return QuickRow{}, err
			}
			if code != 200 {
				return QuickRow{}, fmt.Errorf("%s answered %d", q, code)
			}
			if d := time.Since(t0); d > worst {
				worst, worstQuery = d, q
			}
		}
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(worstQuery, "/"), "?")
	return QuickRow{ID: "T1", Verdict: band(worst, 200*time.Millisecond, time.Second),
		Measured: fmt.Sprintf("Slowest %d ms uncached on store %d (%s)", worst.Milliseconds(), store, name)}, nil
}

// quickFreshness adds orders far in the past, where no other write lands, and
// waits for each to appear. The probes are deleted afterwards.
func (c *Core) quickFreshness(ctx context.Context, base, auth string, store int32) (QuickRow, error) {
	var worst time.Duration
	app := fmt.Sprintf("%s/app/tenants/%d/orders", c.Opt.StoreAppURL, store)
	at := time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(time.Now().Unix()%100000) * time.Hour)
	for i := 0; i < 5; i++ {
		probe := at.Add(time.Duration(i) * time.Minute)
		body, _ := json.Marshal(map[string]string{"created_at": probe.Add(30 * time.Second).Format(time.RFC3339)})
		t0 := time.Now()
		id, err := quickInsert(ctx, app, body)
		if err != nil {
			return QuickRow{}, err
		}
		defer quickDelete(fmt.Sprintf("%s/%d", app, id))
		v := url.Values{"by": {"status"}, "from": {probe.Format(time.RFC3339)}, "to": {probe.Add(time.Minute).Format(time.RFC3339)}}
		q := fmt.Sprintf("%s/tenants/%d/orders/breakdown?%s", base, store, v.Encode())
		for {
			_, b, err := quickGet(ctx, q, auth)
			if err != nil {
				return QuickRow{}, fmt.Errorf("new order did not appear: %w", err)
			}
			if strings.Contains(string(b), `"placed"`) {
				break
			}
			select {
			case <-ctx.Done():
				return QuickRow{}, fmt.Errorf("new order did not appear: %w", ctx.Err())
			case <-time.After(25 * time.Millisecond):
			}
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	return QuickRow{ID: "T2", Verdict: band(worst, 30*time.Second, 5*time.Minute),
		Measured: fmt.Sprintf("Slowest of 5 new orders visible in %.1f s", worst.Seconds())}, nil
}

func quickInsert(ctx context.Context, url string, body []byte) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("store app: %w", err)
	}
	defer resp.Body.Close()
	var o struct {
		OrderID int64 `json:"order_id"`
	}
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&o) != nil {
		return 0, fmt.Errorf("store app answered %d to a new order", resp.StatusCode)
	}
	return o.OrderID, nil
}

func quickDelete(url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// quickCorrectness runs a small batch of the same fenced checks as Verify everything.
func (c *Core) quickCorrectness(ctx context.Context) (QuickRow, error) {
	const n = 40
	c.batchMu.Lock()
	if c.batch.Running {
		c.batchMu.Unlock()
		return QuickRow{}, fmt.Errorf("Verify everything is already running")
	}
	c.batch = BatchStatus{Running: true, Total: n, StartedAt: time.Now()}
	c.batchMu.Unlock()
	c.runBatch(n)
	c.batchMu.Lock()
	b := c.batch
	c.batchMu.Unlock()
	verdict := "Great"
	if b.Exact < n {
		verdict = "Not met"
	}
	return QuickRow{ID: "T4", Verdict: verdict, Measured: fmt.Sprintf("%d of %d exact, while writes ran", b.Exact, n)}, ctx.Err()
}

func band(d, great, good time.Duration) string {
	switch {
	case d < great:
		return "Great"
	case d < good:
		return "Good"
	}
	return "Not met"
}
