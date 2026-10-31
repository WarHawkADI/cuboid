package query

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"cuboid/internal/audit"
	"cuboid/internal/auth"
	"cuboid/internal/config"
	"cuboid/internal/store"
	"cuboid/internal/timeutil"
)

// VerifyFunc runs a check of a request against the main database.
type VerifyFunc func(ctx context.Context, t Target, req *Request) (any, *Problem)

// API serves the PRD's REST shape.
type API struct {
	Svc     *Service
	Keys    *auth.Keys
	Members *auth.Membership
	Verify  VerifyFunc
	Hub     *Hub
}

// Register adds the data routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	for _, kind := range []string{"tenants", "companies"} {
		company := kind == "companies"
		mux.HandleFunc("GET /"+kind+"/{id}/meta", a.guarded(func(w http.ResponseWriter, r *http.Request) { a.meta(w, r, company) }))
		mux.HandleFunc("GET /"+kind+"/{id}/{name}/{op}", a.guarded(func(w http.ResponseWriter, r *http.Request) { a.data(w, r, company) }))
		mux.HandleFunc("POST /"+kind+"/{id}/verify", a.guarded(func(w http.ResponseWriter, r *http.Request) { a.verify(w, r, company) }))
	}
}

// guardedWriter refuses any body that did not pass the response guard or is
// not an error problem. A handler that forgets the guard cannot leak data.
type guardedWriter struct {
	http.ResponseWriter
	approved bool
	blocked  bool
}

func (g *guardedWriter) Write(b []byte) (int, error) {
	if !g.approved {
		if !g.blocked {
			g.blocked = true
			g.ResponseWriter.WriteHeader(http.StatusInternalServerError)
		}
		return 0, fmt.Errorf("response body written without passing the response guard")
	}
	return g.ResponseWriter.Write(b)
}

func (g *guardedWriter) Flush() {
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *API) guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(&guardedWriter{ResponseWriter: w}, r)
	}
}

// WriteProblem writes an error. Problems carry no tenant data.
func WriteProblem(w http.ResponseWriter, p *Problem) {
	if g, ok := w.(*guardedWriter); ok {
		g.approved = true
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if p.Status == 429 || p.Status == 503 {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p)
}

var notFound = &Problem{Status: 404, Type: "not_found", Title: "Not found",
	Detail: "No store or company with this id is available with this key."}

// authenticate resolves the key and the path selector into a Target.
func (a *API) authenticate(w http.ResponseWriter, r *http.Request, company bool) (Target, bool) {
	d := a.Svc.Drill
	d.checked(LayerKeyScope)
	if r.URL.Query().Has("key") || r.URL.Query().Has("api_key") {
		d.blocked(LayerKeyScope)
		WriteProblem(w, &Problem{Status: 401, Type: "unauthenticated", Title: "Key in the URL",
			Detail: "Keys go in the Authorization header, never in the URL, where proxies and logs would keep them.",
			Hint:   "Authorization: Bearer <key>. Run make keys or see README, Demo keys."})
		return Target{}, false
	}
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	scope, valid := auth.Scope{}, false
	if ok {
		scope, valid = a.Keys.Authenticate(strings.TrimSpace(token))
	}
	if !valid {
		d.blocked(LayerKeyScope)
		WriteProblem(w, &Problem{Status: 401, Type: "unauthenticated", Title: "Missing or unknown key",
			Detail: "Every request needs a store key, so one store can never read another's numbers.",
			Hint:   "Send Authorization: Bearer <key>. Run make keys or see README, Demo keys."})
		return Target{}, false
	}
	if !a.Svc.Limits.AllowKey(scope.Prefix) {
		WriteProblem(w, &Problem{Status: 429, Type: "rate_limited", Title: "Limited",
			Detail: fmt.Sprintf("This key is sending more than %.0f requests a second.", a.Svc.Limits.Config().KeyRate)})
		return Target{}, false
	}
	d.checked(LayerPathCheck)
	id, ok := auth.ParseID(r.PathValue("id"))
	var tenants []int32
	if ok {
		tenants, ok = a.Members.Resolve(scope, company, id)
	}
	if !ok {
		d.blocked(LayerPathCheck)
		WriteProblem(w, notFound)
		return Target{}, false
	}
	t := Target{Scope: scope, Company: company, ID: id, Tenants: tenants}
	if company {
		t.Partition = fmt.Sprintf("company:%d", id)
	} else {
		t.Partition = fmt.Sprintf("store:%d", id)
	}
	return t, true
}

// resolve maps {name}/{op} to a source endpoint or a top list.
func resolve(cfg *config.Config, name, op string) (*config.Source, *config.TopList, bool) {
	if op == EpTop {
		for _, src := range cfg.FactSources() {
			if tl := src.Top.Get(name); tl != nil {
				return src, tl, true
			}
		}
		return nil, nil, false
	}
	src := cfg.Sources.Get(name)
	if src == nil || src.Time == "" {
		return nil, nil, false
	}
	switch op {
	case EpCount, EpSummary, EpTrend, EpBreakdown, "stream":
		return src, nil, true
	}
	return nil, nil, false
}

func (a *API) data(w http.ResponseWriter, r *http.Request, company bool) {
	start := time.Now()
	t, ok := a.authenticate(w, r, company)
	if !ok {
		return
	}
	act := a.Svc.Active()
	name, op := r.PathValue("name"), r.PathValue("op")
	src, tl, ok := resolve(act.Cfg, name, op)
	if !ok {
		WriteProblem(w, &Problem{Status: 404, Type: "not_found", Title: "Not found",
			Detail: fmt.Sprintf("There is no /%s/%s endpoint.", name, op), Hint: "GET /tenants/{id}/meta lists the endpoints."})
		return
	}
	if op == "stream" {
		a.stream(w, r, t)
		return
	}
	req, prob := act.Parser.Parse(op, src, tl, r.URL.Query(), company)
	if prob != nil {
		WriteProblem(w, prob)
		return
	}
	opt := ExecOptions{NoCache: t.Scope.Kind == auth.Operator && r.Header.Get("X-Cuboid-Bench") == "uncached"}
	resp, prob := a.Svc.Execute(r.Context(), t, req, opt)
	status, rows, cacheStatus := 200, 0, ""
	if prob != nil {
		WriteProblem(w, prob)
		status = prob.Status
	} else {
		cacheStatus = resp.Meta.Cache
		rows = len(resp.tenants)
		status = a.writeData(w, t, resp)
	}
	a.Svc.Latency.Observe(req.Echo["endpoint"].(string), time.Since(start))
	a.Svc.Audit.Access(audit.Access{At: start, Scope: string(t.Scope.Kind), ScopeID: t.Scope.ID, Tenants: t.Tenants,
		KeyPrefix: t.Scope.Prefix, Endpoint: r.URL.Path, Query: r.URL.RawQuery, Status: status, ResultRows: rows,
		Cache: cacheStatus, DurationMS: float32(time.Since(start).Microseconds()) / 1000})
}

// writeData is the response guard (layer 6): the stores the data came from
// must be within the key's scope for this request, or nothing is sent.
func (a *API) writeData(w http.ResponseWriter, t Target, resp *Response) int {
	a.Svc.Drill.checked(LayerResponseGuard)
	for _, x := range resp.tenants {
		if !contains(t.Tenants, x) {
			a.Svc.Drill.blocked(LayerResponseGuard)
			a.Svc.CrossTenantBlocked.Add(1)
			a.Svc.Audit.Event(audit.Event{Kind: "cross_tenant_response_blocked", Severity: "P1", Tenant: t.ID, KeyPrefix: t.Scope.Prefix,
				Detail: fmt.Sprintf("response guard (layer 6) stopped an answer built from store %d for a request scoped to %v", x, t.Tenants)})
			WriteProblem(w, &Problem{Status: 500, Type: "internal", Title: "Internal error", Detail: "The answer failed an isolation check and was not sent."})
			return 500
		}
	}
	resp = resp.clone()
	if t.Company {
		id := t.ID
		resp.CompanyID, resp.Stores = &id, t.Tenants
	} else {
		id := t.ID
		resp.TenantID = &id
	}
	if g, ok := w.(*guardedWriter); ok {
		g.approved = true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
	return 200
}

// meta lists what can be asked, generated from config. Dimension values are
// read from this store's own cells, under row-level security.
func (a *API) meta(w http.ResponseWriter, r *http.Request, company bool) {
	t, ok := a.authenticate(w, r, company)
	if !ok {
		return
	}
	act := a.Svc.Active()
	cfg := act.Cfg
	type dimInfo struct {
		Name   string   `json:"name"`
		Kind   string   `json:"kind"`
		Values []string `json:"values,omitempty"`
	}
	type rollupInfo struct {
		Name   string            `json:"name"`
		Dims   []string          `json:"dims"`
		Grains map[string]string `json:"grains"`
		Ready  bool              `json:"ready"`
	}
	type sourceInfo struct {
		Name       string            `json:"name"`
		Measures   map[string]string `json:"measures"`
		Derived    []string          `json:"derived"`
		Dimensions []dimInfo         `json:"dimensions"`
		Top        []string          `json:"top"`
		Endpoints  []string          `json:"endpoints"`
	}
	var sources []sourceInfo
	var tenantsInData []int32
	err := a.Svc.Store.ReadTx(r.Context(), t.Tenants, store.ReadOptions{}, func(tx pgx.Tx, _ store.Freshness) error {
		for _, src := range cfg.FactSources() {
			si := sourceInfo{Name: src.Name, Measures: map[string]string{}}
			for _, m := range src.Measures.Values() {
				desc := m.Type
				if m.Unit != "" {
					desc += " (" + m.Unit + ")"
				}
				si.Measures[m.Name] = desc
			}
			for _, d := range src.Derived.Values() {
				si.Derived = append(si.Derived, d.Name)
			}
			for _, d := range src.Dimensions.Values() {
				di := dimInfo{Name: d.Name, Kind: "text"}
				if d.IsKey() {
					di.Kind = "id"
				} else {
					vals, ts, err := a.dimValues(r.Context(), tx, cfg, src, d.Name, t.Tenants)
					if err != nil {
						return err
					}
					di.Values = vals
					tenantsInData = append(tenantsInData, ts...)
				}
				si.Dimensions = append(si.Dimensions, di)
			}
			for _, tl := range src.Top.Values() {
				si.Top = append(si.Top, tl.Name)
				si.Endpoints = append(si.Endpoints, "GET /{scope}/{id}/"+tl.Name+"/top")
			}
			for _, ep := range []string{EpCount, EpSummary, EpTrend, EpBreakdown, "stream"} {
				si.Endpoints = append(si.Endpoints, "GET /{scope}/{id}/"+src.Name+"/"+ep)
			}
			sources = append(sources, si)
		}
		return nil
	})
	if err != nil {
		WriteProblem(w, a.Svc.problemFor(err, t))
		return
	}
	var rollups []rollupInfo
	for _, ro := range cfg.Rollups.Values() {
		rollups = append(rollups, rollupInfo{Name: ro.Name, Dims: ro.Dims, Grains: ro.GrainSpec, Ready: act.Planner.Ready == nil || act.Planner.Ready(ro.Name)})
	}
	resp := &Response{Query: map[string]any{"endpoint": "meta"}, tenants: tenantsInData,
		Data: map[string]any{"sources": sources, "rollups": rollups, "presets": timeutil.Presets, "timezone": a.Svc.Loc.String()},
		Freshness: a.Svc.freshness(a.Svc.Fresh())}
	a.writeData(w, t, resp)
}

// dimValues returns the values of a text dimension present in the given
// stores' month cells.
func (a *API) dimValues(ctx context.Context, tx pgx.Tx, cfg *config.Config, src *config.Source, dim string, tenants []int32) ([]string, []int32, error) {
	var ro *config.Rollup
	for _, r := range cfg.RollupsFor(src.Name) {
		if i := r.DimIndex(dim); i >= 0 && (ro == nil || len(r.Dims) < len(ro.Dims)) {
			ro = r
		}
	}
	if ro == nil {
		return nil, nil, nil
	}
	sql := fmt.Sprintf(`SELECT DISTINCT tenant_id, k%d FROM %s WHERE tenant_id = ANY($1) AND rollup = $2 AND grain = (SELECT max(grain) FROM %s WHERE tenant_id = ANY($1) AND rollup = $2)`,
		ro.DimIndex(dim)+1, store.Names{}.Cells(src.Name), store.Names{}.Cells(src.Name))
	rows, err := tx.Query(ctx, sql, tenants, ro.ID)
	if err != nil {
		return nil, nil, err
	}
	var ids []int64
	var ts []int32
	for rows.Next() {
		var t int32
		var id int64
		if err := rows.Scan(&t, &id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ts = append(ts, t)
		if id != 0 {
			ids = append(ids, id)
		}
	}
	rows.Close()
	seen := map[string]bool{}
	var vals []string
	for _, id := range ids {
		v, err := a.Svc.Dict.ValueOrLoad(ctx, tx, id)
		if err != nil {
			return nil, nil, err
		}
		if !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	sort.Strings(vals)
	return vals, ts, rows.Err()
}

type verifyBody struct {
	Endpoint string            `json:"endpoint"`
	Params   map[string]string `json:"params"`
}

func (a *API) verify(w http.ResponseWriter, r *http.Request, company bool) {
	t, ok := a.authenticate(w, r, company)
	if !ok {
		return
	}
	var body verifyBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		WriteProblem(w, bad("the body must be JSON like {\"endpoint\": \"orders/count\", \"params\": {\"range\": \"last_7d\"}}", ""))
		return
	}
	name, op, _ := strings.Cut(body.Endpoint, "/")
	act := a.Svc.Active()
	src, tl, ok := resolve(act.Cfg, name, op)
	if !ok || op == "stream" {
		WriteProblem(w, bad(fmt.Sprintf("endpoint %q cannot be verified", body.Endpoint), "for example orders/count, orders/trend, products/top, orders/breakdown"))
		return
	}
	v := map[string][]string{}
	for k, val := range body.Params {
		v[k] = []string{val}
	}
	req, prob := act.Parser.Parse(op, src, tl, v, company)
	if prob != nil {
		WriteProblem(w, prob)
		return
	}
	result, prob := a.Verify(r.Context(), t, req)
	if prob != nil {
		WriteProblem(w, prob)
		return
	}
	// The verification result holds both answers for this request's stores only.
	a.writeData(w, t, &Response{Query: req.Echo, From: timeutil.Format(req.Query.From, a.Svc.Loc), To: timeutil.Format(req.Query.To, a.Svc.Loc),
		Data: result, tenants: t.Tenants, Freshness: a.Svc.freshness(a.Svc.Fresh())})
}
