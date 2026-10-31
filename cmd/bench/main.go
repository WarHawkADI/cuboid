// Command bench measures Cuboid against the PRD: the five target tests, the
// golden acceptance checks, the isolation suite, the chaos loop and the
// benchmark steps. Every published number comes from an artifact it writes.
//
//	bench speed | freshness | safety | correctness | growth | targets
//	bench demo-check | redteam | isolation [-requests N] | chaos [-n N] | steps
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	cuboidURL  = env("BENCH_CUBOID_URL", "http://localhost:8080")
	appURL     = env("BENCH_STOREAPP_URL", "http://localhost:8081")
	loadgenURL = env("BENCH_LOADGEN_URL", "http://localhost:8082")
	artifacts  = env("BENCH_ARTIFACTS", "artifacts")
	stopCmd    = env("BENCH_CUBOID_STOP", "docker compose stop cuboid")
	startCmd   = env("BENCH_CUBOID_START", "docker compose start cuboid")
	client     = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConns: 2000, MaxIdleConnsPerHost: 2000}}
)

type demoKey struct {
	Kind   string  `json:"kind"`
	ID     int32   `json:"id"`
	Name   string  `json:"name"`
	Key    string  `json:"key"`
	Stores []int32 `json:"stores"`
}

type world struct {
	op        string
	stores    []demoKey // largest first
	companies []demoKey
	counts    map[int32]int64
	tier      string
}

func loadWorld() (*world, error) {
	var body struct {
		Keys []demoKey `json:"keys"`
	}
	if err := getJSON(cuboidURL+"/api/demo/keys", "", &body); err != nil {
		return nil, fmt.Errorf("read demo keys (is CUBOID_DEMO_MODE on and Cuboid up?): %w", err)
	}
	w := &world{counts: map[int32]int64{}}
	for _, k := range body.Keys {
		switch k.Kind {
		case "operator":
			w.op = k.Key
		case "store":
			w.stores = append(w.stores, k)
		case "company":
			w.companies = append(w.companies, k)
		}
	}
	var total int64
	for _, s := range w.stores {
		var r struct {
			Data struct {
				Count int64 `json:"count"`
			} `json:"data"`
		}
		if err := getJSON(fmt.Sprintf("%s/tenants/%d/orders/count?range=all", cuboidURL, s.ID), w.op, &r); err != nil {
			return nil, err
		}
		w.counts[s.ID] = r.Data.Count
		total += r.Data.Count
	}
	sort.Slice(w.stores, func(i, j int) bool { return w.counts[w.stores[i].ID] > w.counts[w.stores[j].ID] })
	switch {
	case total >= 5_000_000:
		w.tier = "main"
	case total >= 500_000:
		w.tier = "small"
	default:
		w.tier = "dev"
	}
	return w, nil
}

func getJSON(url, key string, v any) error {
	req, _ := http.NewRequest("GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	return json.Unmarshal(b, v)
}

func postJSON(url, key string, body, v any) (int, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if v != nil {
		json.Unmarshal(rb, v)
	}
	return resp.StatusCode, nil
}

// fenced runs a check against the main database and reports its verdict.
func (w *world) check(scope, endpoint string, params map[string]string) (bool, string, error) {
	var r struct {
		Data struct {
			Exact      bool     `json:"exact"`
			Rows       int      `json:"rows_compared"`
			Mismatches []string `json:"mismatches"`
		} `json:"data"`
		Detail string `json:"detail"`
	}
	code, err := postJSON(cuboidURL+scope+"/verify", w.op, map[string]any{"endpoint": endpoint, "params": params}, &r)
	if err != nil {
		return false, "", err
	}
	if code != 200 {
		return false, "", fmt.Errorf("verify %s: %d %s", endpoint, code, r.Detail)
	}
	return r.Data.Exact, fmt.Sprintf("%d rows; %s", r.Data.Rows, strings.Join(r.Data.Mismatches, "; ")), nil
}

func environment() map[string]string {
	return map[string]string{"os": runtime.GOOS, "cpus": fmt.Sprint(runtime.NumCPU()), "go": runtime.Version(),
		"deployment": env("BENCH_DEPLOYMENT", "every service on one machine"), "postgres": "16"}
}

func writeArtifact(kind, name string, v any) string {
	dir := filepath.Join(artifacts, kind)
	os.MkdirAll(dir, 0o755)
	if name == "" {
		name = time.Now().Format("20060102T150405")
	}
	p := filepath.Join(dir, name+".json")
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", p)
	return p
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}

func shell(cmdline string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", cmdline)
	} else {
		cmd = exec.Command("sh", "-c", cmdline)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func waitHealthy(d time.Duration) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		var h struct {
			State string `json:"state"`
		}
		if getJSON(cuboidURL+"/healthz", "", &h) == nil && h.State == "streaming" {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("Cuboid was not streaming within %s", d)
}

func main() {
	if len(os.Args) < 2 {
		fatal(fmt.Errorf("usage: bench speed|freshness|safety|correctness|growth|targets|scorecard|report|demo-check|redteam|isolation|chaos|steps|rebuild"))
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	requests := fs.Int("requests", 1_000_000, "isolation: requests to send")
	n := fs.Int("n", 20, "chaos: kill -9 iterations")
	phase := fs.Duration("phase", 60*time.Second, "safety and steps: measured time per phase")
	viewers := fs.Int("viewers", 1000, "dashboards for the loaded phases")
	quick := fs.Bool("quick", false, "shorter runs, for the Run now button and development")
	fs.Parse(os.Args[2:])
	if cmd == "report" {
		writeReport()
		return
	}
	w, err := loadWorld()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("Tier %s: %d stores, %d companies\n", w.tier, len(w.stores), len(w.companies))
	ctx := context.Background()
	switch cmd {
	case "speed":
		runSpeed(w, *viewers, *quick)
	case "freshness":
		runFreshness(w, *quick)
	case "safety":
		runSafety(w, *phase, *viewers)
	case "correctness":
		runCorrectness(w, *quick)
	case "growth":
		runGrowth()
	case "targets":
		runSpeed(w, *viewers, *quick)
		runFreshness(w, *quick)
		runCorrectness(w, *quick)
		runSafety(w, *phase, *viewers)
		runGrowth()
		writeScorecard(w)
	case "scorecard":
		writeScorecard(w)
	case "demo-check":
		if !runDemoCheck(ctx, w) {
			os.Exit(1)
		}
	case "isolation":
		if !runIsolation(ctx, w, *requests) {
			os.Exit(1)
		}
	case "chaos":
		if !runChaos(w, *n) {
			os.Exit(1)
		}
	case "steps":
		runSteps(w, *phase)
	case "rebuild":
		rebuildFromMain(w)
	case "redteam":
		if !runRedTeam(ctx, w) {
			os.Exit(1)
		}
	default:
		fatal(fmt.Errorf("unknown command %q", cmd))
	}
}
