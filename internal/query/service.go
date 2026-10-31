// Package query serves the PRD's dashboard API from the aggregate store.
// Nothing here can reach the main database: the service holds only the AGG
// reader pool, every read runs under row-level security, every result row's
// tenant is asserted, and every response passes the guard before it leaves.
package query

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"cuboid/internal/audit"
	"cuboid/internal/auth"
	"cuboid/internal/cache"
	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/limiter"
	"cuboid/internal/monitor"
	"cuboid/internal/planner"
	"cuboid/internal/store"
	"cuboid/internal/timeutil"
)

// Target is the authorised selection of a request: which store or company
// the path names, and the stores the key may read through it.
type Target struct {
	Scope     auth.Scope
	Company   bool
	ID        int32
	Tenants   []int32
	Partition string // cache and limiter partition: "store:42" or "company:7"
}

// Active is the configuration and planner in use. It is swapped atomically
// on a config reload.
type Active struct {
	Cfg     *config.Config
	Planner *planner.Planner
	Parser  *Parser
}

// Service executes requests.
type Service struct {
	active  atomic.Pointer[Active]
	Store   *store.Store
	Dict    *store.Dict
	Loc     *time.Location
	Now     func() time.Time
	Cache   *cache.Cache
	Hist    *cache.History
	Limits  *limiter.Limiter
	Members *auth.Membership
	Audit   *audit.Log
	Stats   *Stats
	Drill   *Drill
	Latency *monitor.Latency
	// Fresh reports the state of the latest flush, for answers served from cache.
	Fresh func() store.Freshness
	sf    singleflight.Group

	StaleServed        atomic.Int64
	CrossTenantBlocked atomic.Int64
	Coalesced          atomic.Int64
}

// SetActive installs a configuration.
func (s *Service) SetActive(a *Active) { s.active.Store(a) }

// Active returns the configuration in use.
func (s *Service) Active() *Active { return s.active.Load() }

// ExecOptions alter one execution.
type ExecOptions struct {
	NoCache bool // operator bench header: measure the uncached path
}

// errCrossTenant is returned when a store result row belongs to a tenant
// outside the request's scope (layer 5). It never becomes a response.
type errCrossTenant struct {
	got  int32
	want []int32
}

func (e *errCrossTenant) Error() string {
	return fmt.Sprintf("result row for store %d outside the requested stores %v", e.got, e.want)
}

// topLimit asks the store for only the largest groups. total comes back with
// them: the measure over all groups, which shares are computed from.
type topLimit struct {
	measure string
	n       int
	total   int64
}

// pushdown returns a limit when a top-N list is keyed by ids, the case with
// many groups: products and customers. Text dimensions have few groups and sort
// by label on ties, which the database cannot do from dictionary ids.
func pushdown(req *Request, plan *planner.Plan) *topLimit {
	if req.Endpoint != EpTop || plan.Rollup == nil || !plan.Source.Dimensions.Get(req.Top.Dim).IsKey() {
		return nil
	}
	return &topLimit{measure: req.Measure, n: req.N}
}

// staleServe bounds how old an answer may be served while its refresh runs.
const staleServe = 2 * time.Second

// refresh recomputes an entry in the background, once per data version.
func (s *Service) refresh(t Target, req *Request, plan *planner.Plan, key string, cells, edgeRows float64, heavy bool) {
	versions := s.Hist.Snapshot(t.Tenants)
	flightKey := fmt.Sprintf("%s|%s|%v", t.Partition, key, versions)
	s.sf.DoChan(flightKey, func() (result any, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("query panicked: %v", p)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		release, err := s.Limits.Acquire(ctx, t.Partition, heavy)
		if err != nil {
			return nil, err // limited: the next request tries again
		}
		defer release()
		r, err := s.compute(ctx, t, req, plan, cells, edgeRows, heavy)
		if err == nil {
			s.Cache.Put(t.Partition, key, cacheEntry(req, versions, r))
		}
		return r, err
	})
}

// errWrongShape means a store result does not have this query's columns: the
// result of another query was delivered to this one.
var errWrongShape = errors.New("the store result does not have this query's shape")

// Execute answers a request for a target.
func (s *Service) Execute(ctx context.Context, t Target, req *Request, opt ExecOptions) (*Response, *Problem) {
	start := time.Now()
	act := s.Active()
	req.Query.Tenants = t.Tenants
	plan, err := act.Planner.Plan(&req.Query)
	var np *planner.NotPreaggregated
	if errors.As(err, &np) {
		return nil, &Problem{Status: 422, Type: "not_preaggregated", Title: "No rollup covers this query", Detail: np.Hint,
			Hint: "Add a rollup in config/metrics.yaml; it is built from the mirror and served without a restart."}
	}
	if err != nil {
		return nil, &Problem{Status: 400, Type: "invalid_query", Title: "Invalid query", Detail: err.Error()}
	}
	cells, edgeRows := s.Stats.Cost(plan, t.Tenants, s.Loc)
	lim := s.Limits.Config()
	if edgeRows > float64(lim.MaxEdgeRows) {
		return nil, &Problem{Status: 422, Type: "not_preaggregated", Title: "This range needs too many raw rows",
			Detail: fmt.Sprintf("about %.0f rows would be read from the mirror because rollup %s has no %s cells for part of the range", edgeRows, plan.Rollup.Name, finestMissing(plan)),
			Hint:   "Use whole hours or days, a shorter range, or add finer grains to the rollup in config/metrics.yaml."}
	}
	heavy := cells+edgeRows > float64(lim.HeavyCost)

	key := cacheKey(req)
	if s.Drill.Mode() == DrillSharedCache {
		if r, ok := s.Drill.sharedGet(key); ok {
			for _, x := range r.tenants {
				if !contains(t.Tenants, x) {
					s.Drill.injected() // this answer would cross stores without the guard
					break
				}
			}
			return r.withCache("hit"), nil
		}
	}
	// During a drill every request reaches the faulty path.
	useCache := !opt.NoCache && s.Drill.Mode() == DrillOff
	if useCache {
		s.Drill.checked(LayerCachePartition)
		if e, ok := s.Cache.Get(t.Partition, key); ok {
			r := e.Value.(*Response).withCache("hit")
			r.Freshness = s.freshness(s.Fresh())
			r.Meta.Timing.Total = ms(time.Since(start))
			return r, nil
		}
		// New data arrived. On a busy store that is every flush, and making
		// every viewer wait for the same recomputation is what hurts: serve the
		// answer we have, exact as of when it was computed and labelled so, and
		// refresh it once in the background.
		if e, ok := s.Cache.Peek(t.Partition, key); ok && time.Since(e.Created) < staleServe {
			r := e.Value.(*Response).withCache("stale")
			r.Freshness = s.freshness(r.state)
			r.Freshness.Stale = true
			r.Meta.Timing.Total = ms(time.Since(start))
			s.StaleServed.Add(1)
			s.refresh(t, req, plan, key, cells, edgeRows, heavy)
			return r, nil
		}
	}
	versions := s.Hist.Snapshot(t.Tenants)
	flightKey := fmt.Sprintf("%s|%s|%v", t.Partition, key, versions)
	if opt.NoCache {
		flightKey += "|" + strconv.FormatInt(rand.Int64(), 36)
	}
	v, err, shared := s.sf.Do(flightKey, func() (result any, err error) {
		// singleflight re-raises a panic where nothing can recover it, which
		// would take the whole process down with one bad request.
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("query panicked: %v", p)
			}
		}()
		// Detached: viewers share this computation, so the first viewer
		// cancelling must not fail everyone else who is waiting on it.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		release, err := s.Limits.Acquire(cctx, t.Partition, heavy)
		if err != nil {
			return nil, err
		}
		defer release()
		return s.compute(cctx, t, req, plan, cells, edgeRows, heavy)
	})
	if err != nil {
		// The store could not answer in time (for example at the peak of a
		// rebuild): the newest answer we hold, labelled with its true age, beats
		// an error. Isolation failures are never served around.
		var ct *errCrossTenant
		if useCache && !errors.As(err, &ct) && !errors.Is(err, errWrongShape) && !errors.Is(err, store.ErrNoTenantContext) {
			if e, ok := s.Cache.Peek(t.Partition, key); ok {
				r := e.Value.(*Response).withCache("stale")
				r.Freshness = s.freshness(r.state)
				r.Freshness.Stale = true
				r.Meta.Timing.Total = ms(time.Since(start))
				s.StaleServed.Add(1)
				return r, nil
			}
		}
		return nil, s.problemFor(err, t)
	}
	r := v.(*Response)
	if shared {
		s.Coalesced.Add(1)
		r = r.withCache("coalesced")
	}
	if useCache && !shared {
		s.Cache.Put(t.Partition, key, cacheEntry(req, versions, r))
	}
	if s.Drill.Mode() == DrillSharedCache {
		s.Drill.sharedPut(key, r)
	}
	r = r.clone()
	r.Meta.Timing.Total = ms(time.Since(start))
	return r, nil
}

func finestMissing(pl *planner.Plan) string {
	for _, g := range []timeutil.Grain{timeutil.GrainHour, timeutil.GrainDay} {
		if pl.Rollup.HasGrain(g) {
			continue
		}
		return g.String()
	}
	return "hour"
}

func (s *Service) problemFor(err error, t Target) *Problem {
	var lim *limiter.ErrLimited
	var ct *errCrossTenant
	switch {
	case errors.As(err, &lim):
		return &Problem{Status: 429, Type: "rate_limited", Title: "Limited", Detail: lim.Error(), Hint: "Light queries from this store are not affected."}
	case errors.Is(err, limiter.ErrSaturated):
		return &Problem{Status: 503, Type: "saturated", Title: "Busy", Detail: "The aggregate store is at its concurrency limit. Try again in 1 second."}
	case errors.As(err, &ct):
		s.CrossTenantBlocked.Add(1)
		s.Drill.blocked(LayerResultCheck)
		s.Audit.Event(audit.Event{Kind: "cross_tenant_result_blocked", Severity: "P1", Tenant: t.ID, KeyPrefix: t.Scope.Prefix,
			Detail: "result assertion (layer 5) stopped a store result containing another store's rows: " + ct.Error()})
		return &Problem{Status: 500, Type: "internal", Title: "Internal error", Detail: "The answer failed an isolation check and was not sent."}
	case errors.Is(err, errWrongShape):
		s.Drill.blocked(LayerResultCheck)
		s.Audit.Event(audit.Event{Kind: "wrong_result_blocked", Severity: "P1", Tenant: t.ID, KeyPrefix: t.Scope.Prefix, Detail: "result assertion (layer 5) stopped another query's result"})
		return &Problem{Status: 500, Type: "internal", Title: "Internal error", Detail: "The answer failed an isolation check and was not sent."}
	case errors.Is(err, store.ErrNoTenantContext):
		s.Drill.blocked(LayerRowSecurity)
		s.Audit.Event(audit.Event{Kind: "tenant_context_missing", Severity: "P1", Tenant: t.ID, KeyPrefix: t.Scope.Prefix,
			Detail: "an aggregate-store read ran without its tenant setting; row-level security returned no rows and the answer was refused"})
		return &Problem{Status: 500, Type: "internal", Title: "Internal error", Detail: "The answer failed an isolation check and was not sent."}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &Problem{Status: 503, Type: "cancelled", Title: "Cancelled", Detail: "The request was cancelled before it finished."}
	}
	s.Audit.Event(audit.Event{Kind: "query_failed", Severity: "P3", Tenant: t.ID, Detail: err.Error()})
	return &Problem{Status: 500, Type: "internal", Title: "Internal error", Detail: "The query failed. Try again; if it persists, see the server log."}
}

// compute reads the answer from AGG under RLS and shapes it.
func (s *Service) compute(ctx context.Context, t Target, req *Request, plan *planner.Plan, cells, edgeRows float64, heavy bool) (*Response, error) {
	act := s.Active()
	var resp *Response
	storeStart := time.Now()
	opt := store.ReadOptions{SkipTenantContext: s.Drill.Mode() == DrillMissingContext && rand.IntN(3) == 0}
	if opt.SkipTenantContext {
		s.Drill.injected()
	}
	s.Drill.checked(LayerRowSecurity)
	s.Drill.checked(LayerResultCheck)
	err := s.Store.ReadTx(ctx, t.Tenants, opt, func(tx pgx.Tx, f store.Freshness) error {
		top := pushdown(req, plan)
		res, err := s.read(ctx, tx, act.Cfg, plan, t, top)
		if err != nil {
			return err
		}
		storeMS := ms(time.Since(storeStart))
		data, err := s.shape(ctx, tx, act.Cfg, req, plan, res, t, top)
		if err != nil {
			return err
		}
		resp = &Response{
			Query: req.Echo, From: timeutil.Format(req.Query.From, s.Loc), To: timeutil.Format(req.Query.To, s.Loc),
			Data: data, Freshness: s.freshness(f), tenants: tenantsOf(res), state: f,
			Meta: Meta{Plan: describePlan(plan, s.Loc, cells, edgeRows), Cache: "miss", Exact: true, Heavy: heavy,
				Timing: Timing{Store: storeMS}},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// read runs the plan's SQL and returns the result keyed by raw values.
func (s *Service) read(ctx context.Context, tx pgx.Tx, cfg *config.Config, plan *planner.Plan, t Target, top *topLimit) (planner.Result, error) {
	res := planner.Result{}
	if plan.Rollup == nil {
		return res, nil
	}
	enc := store.Encoder{Cfg: cfg, Dict: s.Dict}
	sql, args, ok, err := store.QuerySQL(plan, enc, s.Loc, store.Names{})
	if err != nil {
		return nil, err
	}
	if !ok {
		return res, nil
	}
	if top != nil {
		sql = store.TopSQL(sql, len(plan.Pos), top.measure, top.n)
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate store query: %w", err)
	}
	type raw struct {
		tenant int32
		bucket int64
		keys   []int64
		vals   []int64
	}
	var raws []raw
	nk, nm := len(plan.Pos), plan.Source.Measures.Len()
	for rows.Next() {
		r := raw{keys: make([]int64, nk), vals: make([]int64, nm)}
		dest := []any{&r.tenant, &r.bucket}
		for i := range r.keys {
			dest = append(dest, &r.keys[i])
		}
		for i := range r.vals {
			dest = append(dest, &r.vals[i])
		}
		if top != nil {
			dest = append(dest, &top.total)
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		raws = append(raws, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Drill B: a pooled client that, after a cancellation, hands this request
	// the previous request's rows. This is the INC-02 mechanism.
	if s.Drill.Mode() == DrillConnectionMixup {
		if prev, ok := s.Drill.swap(raws); ok && rand.IntN(3) == 0 {
			raws = prev.([]raw)
			s.Drill.injected()
		}
	}
	// Layer 5: assert every row before using any of them. The result must also
	// have the shape this query asked for, or it is another query's answer.
	for _, r := range raws {
		if !contains(t.Tenants, r.tenant) {
			return nil, &errCrossTenant{got: r.tenant, want: t.Tenants}
		}
		if len(r.keys) != nk || len(r.vals) != nm {
			return nil, errWrongShape
		}
	}
	for _, r := range raws {
		k := planner.ResultKey{Tenant: r.tenant, Bucket: r.bucket}
		for i, id := range r.keys {
			dim := plan.Source.Dimensions.Get(plan.Query.GroupBy[i])
			switch {
			case id == 0:
				k.K[i] = engine.Null
			case dim.IsKey():
				k.K[i] = strconv.FormatInt(id, 10)
			default:
				v, err := s.Dict.ValueOrLoad(ctx, tx, id)
				if err != nil {
					return nil, err
				}
				k.K[i] = v
			}
		}
		res.Add(k, r.vals, 1)
	}
	return res.DropZeros(), nil
}

func contains(ts []int32, t int32) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func tenantsOf(res planner.Result) []int32 {
	seen := map[int32]bool{}
	var out []int32
	for k := range res {
		if !seen[k.Tenant] {
			seen[k.Tenant] = true
			out = append(out, k.Tenant)
		}
	}
	return out
}

func (s *Service) freshness(f store.Freshness) Freshness {
	asOf := f.AsOf()
	if asOf.IsZero() {
		return Freshness{LSN: f.LSN, Updated: "waiting for the first update"}
	}
	lag := s.Now().Sub(asOf)
	if lag < 0 {
		lag = 0
	}
	return Freshness{AsOf: timeutil.Format(asOf, s.Loc), LSN: f.LSN, LagMS: lag.Milliseconds(),
		Updated: timeutil.Updated(lag, lag > 30*time.Second), Stale: lag > 30*time.Second}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Stats estimate query cost from cell density, refreshed in the background.
type Stats struct {
	mu          sync.RWMutex
	perBucket   map[statKey]float64
	rowsPerHour map[int32]float64
}

type statKey struct {
	tenant int32
	rollup int16
	grain  timeutil.Grain
}

// NewStats returns empty statistics (defaults are used until refreshed).
func NewStats() *Stats {
	return &Stats{perBucket: map[statKey]float64{}, rowsPerHour: map[int32]float64{}}
}

// Refresh recomputes cell density and row rates from AGG.
func (st *Stats) Refresh(ctx context.Context, s *store.Store, cfg *config.Config) error {
	per := map[statKey]float64{}
	rph := map[int32]float64{}
	for _, src := range cfg.FactSources() {
		table := store.Names{}.Cells(src.Name)
		rows, err := s.W.Query(ctx, fmt.Sprintf(`SELECT tenant_id, rollup, grain, count(*)::float8 / greatest(count(DISTINCT bucket), 1)
			FROM %s GROUP BY 1, 2, 3`, table))
		if err != nil {
			return err
		}
		for rows.Next() {
			var k statKey
			var g int16
			var v float64
			if err := rows.Scan(&k.tenant, &k.rollup, &g, &v); err != nil {
				rows.Close()
				return err
			}
			k.grain = timeutil.Grain(g)
			per[k] = v
		}
		rows.Close()
		for _, r := range cfg.RollupsFor(src.Name) {
			if len(r.Dims) != 0 || !r.HasGrain(timeutil.GrainDay) {
				continue
			}
			rows, err := s.W.Query(ctx, fmt.Sprintf(`SELECT tenant_id, sum(%s)::float8, count(DISTINCT bucket) FROM %s
				WHERE rollup = $1 AND grain = $2 AND bucket >= now() - interval '90 days' GROUP BY 1`, store.QuoteIdent(src.CountMeasure), table), r.ID, int16(timeutil.GrainDay))
			if err != nil {
				return err
			}
			for rows.Next() {
				var t int32
				var n float64
				var days int64
				if err := rows.Scan(&t, &n, &days); err != nil {
					rows.Close()
					return err
				}
				if days > 0 {
					rph[t] += n / float64(days*24)
				}
			}
			rows.Close()
			break
		}
	}
	st.mu.Lock()
	st.perBucket, st.rowsPerHour = per, rph
	st.mu.Unlock()
	return nil
}

// Cost estimates the cells and mirror rows a plan reads.
func (st *Stats) Cost(pl *planner.Plan, tenants []int32, loc *time.Location) (cells, edgeRows float64) {
	if pl.Rollup == nil {
		return 0, 0
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	buckets := pl.Buckets(loc)
	for _, t := range tenants {
		for g, n := range buckets {
			d, ok := st.perBucket[statKey{tenant: t, rollup: pl.Rollup.ID, grain: g}]
			if !ok {
				d = 20
			}
			cells += d * float64(n)
		}
		rate, ok := st.rowsPerHour[t]
		if !ok {
			rate = 100
		}
		edgeRows += rate * 2 * pl.EdgeDuration().Hours() // recent hours are busier than the average
	}
	return cells, edgeRows
}

// presetMaxAge bounds how long an answer for a moving range such as last_7d
// is reused. It always echoes the from and to it was computed for.
const presetMaxAge = 30 * time.Second

// cacheKey is per query. A moving preset is keyed by name, not by its instants:
// those change every second, which would make every viewer's refresh a miss.
func cacheKey(req *Request) string {
	if req.Range != "" {
		return req.Stable
	}
	return req.Canonical
}

// cacheEntry records what an answer depends on. For a moving preset any new
// change invalidates it, even one after its to: the viewer asked about now.
func cacheEntry(req *Request, versions map[int32]int64, r *Response) *cache.Entry {
	e := &cache.Entry{Versions: versions, From: req.Query.From, To: req.Query.To, Value: r, Tenants: r.tenants, Created: time.Now()}
	if req.Range != "" {
		e.To = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		e.Expires = time.Now().Add(presetMaxAge)
	}
	return e
}

// checkTop proves the pushed-down top-N against the full result of the same
// snapshot: same rows, same order, same total.
func checkTop(src *config.Source, req *Request, full, top planner.Result, limit *topLimit) error {
	mi := src.MeasureIndex(limit.measure)
	type row struct {
		k planner.ResultKey
		v int64
	}
	var all []row
	var total int64
	for k, v := range full {
		total += v[mi]
		if v[mi] != 0 {
			all = append(all, row{k, v[mi]})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		if all[i].k.Tenant != all[j].k.Tenant {
			return all[i].k.Tenant < all[j].k.Tenant
		}
		a, _ := strconv.ParseInt(all[i].k.K[0], 10, 64)
		b, _ := strconv.ParseInt(all[j].k.K[0], 10, 64)
		return a < b
	})
	if len(all) > limit.n {
		all = all[:limit.n]
	}
	if len(top) != len(all) {
		return fmt.Errorf("top %d: %d rows from the store, %d expected", limit.n, len(top), len(all))
	}
	for _, r := range all {
		if v, ok := top[r.k]; !ok || v[mi] != r.v {
			return fmt.Errorf("top %d: %v should be %d", limit.n, r.k, r.v)
		}
	}
	if len(all) > 0 && limit.total != total {
		return fmt.Errorf("top %d: total %d from the store, %d expected", limit.n, limit.total, total)
	}
	return nil
}

// EvalAt evaluates a plan in a fresh AGG snapshot and calls onSnapshot as
// soon as that snapshot is taken (the fenced verifier then lets the next
// flush proceed). It returns the raw result for exact comparison, and checks
// the request's pushed-down form, if it has one, against it.
func (s *Service) EvalAt(ctx context.Context, t Target, req *Request, plan *planner.Plan, onSnapshot func()) (planner.Result, error) {
	var res planner.Result
	called := false
	err := s.Store.ReadTx(ctx, t.Tenants, store.ReadOptions{}, func(tx pgx.Tx, _ store.Freshness) error {
		onSnapshot()
		called = true
		r, err := s.read(ctx, tx, s.Active().Cfg, plan, t, nil)
		res = r
		if err != nil {
			return err
		}
		if limit := pushdown(req, plan); limit != nil {
			top, err := s.read(ctx, tx, s.Active().Cfg, plan, t, limit)
			if err != nil {
				return err
			}
			return checkTop(plan.Source, req, r, top, limit)
		}
		return nil
	})
	if !called {
		onSnapshot()
	}
	return res, err
}
