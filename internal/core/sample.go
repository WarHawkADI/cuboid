package core

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"cuboid/internal/monitor"
)

var sampleClient = &http.Client{Timeout: 700 * time.Millisecond}

func getJSON(ctx context.Context, url string, v any) bool {
	if url == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	resp, err := sampleClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(v) == nil
}

// sample takes one point for the pressure recorder: the store app's own
// query time (the PRD's safety measure), the dashboards' path and load, and
// how busy the main database is.
func (c *Core) sample(ctx context.Context) monitor.Point {
	p := monitor.Point{T: time.Now().UnixMilli(), Mode: "off"}
	var app struct {
		P50   float64 `json:"p50_ms"`
		P95   float64 `json:"p95_ms"`
		OpsPS float64 `json:"ops_per_s"`
	}
	if getJSON(ctx, c.Opt.StoreAppURL+"/metrics/window", &app) {
		p.AppP50, p.AppP95, p.AppOps = app.P50, app.P95, app.OpsPS
	}
	var lg struct {
		Mode    string  `json:"mode"`
		Viewers int     `json:"viewers"`
		QPS     float64 `json:"qps"`
	}
	if getJSON(ctx, c.Opt.LoadgenURL+"/state", &lg) {
		p.Mode, p.Viewers = lg.Mode, lg.Viewers
		if lg.Mode == "cuboid" {
			p.CuboidQPS = lg.QPS
		}
	}
	var nv struct {
		QPS float64 `json:"qps"`
	}
	if getJSON(ctx, c.Opt.NaiveURL+"/stats", &nv) {
		p.DirectQPS = nv.QPS
	}
	var active int
	if err := c.Main.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND backend_type = 'client backend' AND pid <> pg_backend_pid()").Scan(&active); err == nil {
		p.MainActive = active
	}
	if as := c.freshness().AsOf(); !as.IsZero() {
		p.LagMS = time.Since(as).Milliseconds()
	}
	return p
}
