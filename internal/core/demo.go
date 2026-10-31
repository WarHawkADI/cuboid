package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"cuboid/internal/query"
)

// The demo's bonus B-4 beat: three lines of YAML, added to the real config
// file and applied by the real reload path.
const demoMetricName = "status_payment"
const demoMetricLine = "  status_payment:  {source: orders, dims: [status, payment_method], grains: {hour: 35d, day: all, month: all}}\n"

func (c *Core) adminDemoMetric(w http.ResponseWriter, r *http.Request) {
	if !c.Opt.DemoMode {
		http.NotFound(w, r)
		return
	}
	var b struct {
		Add bool `json:"add"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b)
	raw, err := os.ReadFile(c.Opt.MetricsPath)
	if err != nil {
		query.WriteProblem(w, &query.Problem{Status: 500, Type: "internal", Title: "Cannot read the config", Detail: err.Error()})
		return
	}
	var kept []string
	present := false
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), demoMetricName+":") {
			present = true
			continue
		}
		kept = append(kept, line)
	}
	if b.Add == present {
		writeJSON(w, 200, &ReloadReport{OK: true})
		return
	}
	text := strings.Join(kept, "")
	if b.Add {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += demoMetricLine
	}
	if err := os.WriteFile(c.Opt.MetricsPath, []byte(text), 0o644); err != nil {
		query.WriteProblem(w, &query.Problem{Status: 500, Type: "internal", Title: "Cannot write the config", Detail: err.Error()})
		return
	}
	rep := c.Reload(r.Context())
	c.lastReload.Store(rep)
	status := 200
	if !rep.OK {
		status = 422
	}
	writeJSON(w, status, rep)
}

// adminAppOrder forwards FR-6 demo writes to the store app, so the browser
// only ever talks to Cuboid's origin. Cuboid itself writes nothing to main.
func (c *Core) adminAppOrder(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	c.forward(w, c.Opt.StoreAppURL+"/app/tenants/"+r.PathValue("id")+"/orders", body)
}

func (c *Core) adminAppOrderAction(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("%s/app/tenants/%s/orders/%s", c.Opt.StoreAppURL, r.PathValue("id"), r.PathValue("order"))
	switch r.PathValue("action") {
	case "cancel":
		c.forwardMethod(w, "PATCH", url, []byte(`{"status":"cancelled"}`))
	case "delete":
		c.forwardMethod(w, "DELETE", url, nil)
	default:
		query.WriteProblem(w, &query.Problem{Status: 400, Type: "invalid_parameter", Title: "Unknown action", Detail: "use cancel or delete"})
	}
}
