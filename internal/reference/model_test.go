package reference_test

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/planner"
	"cuboid/internal/reference"
	"cuboid/internal/timeutil"
)

var (
	loc, _   = time.LoadLocation("Asia/Kolkata")
	fixedNow = time.Date(2026, 10, 31, 12, 0, 0, 0, loc)
	statuses = []string{"placed", "shipped", "delivered", "cancelled", "returned"}
	channels = []string{"web", "app", "store"}
	payments = []string{"upi", "card", "cod"}
	cats     = []string{"apparel", "kitchen", "books"}
	cities   = []string{"Pune", "Indore", "Kochi"}
)

func loadConfig(t *testing.T) *config.Config {
	cfg, err := config.Load("../../config/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// world is the system under test: the engine feeding an in-memory model of
// the aggregate store (cells and mirror), exactly as the flusher feeds AGG.
type world struct {
	cfg     *config.Config
	eng     *engine.Engine
	ref     *reference.DB
	delta   *engine.Delta
	cells   map[engine.CellKey][]int64
	mirror  map[string]map[string]*engine.MirrorOp
	nextOID map[int32]int
	nextPID map[int32]int
}

func newWorld(t *testing.T) *world {
	cfg := loadConfig(t)
	w := &world{
		cfg:     cfg,
		eng:     engine.New(cfg, loc, func() time.Time { return fixedNow }, nil),
		ref:     reference.New(cfg, loc),
		delta:   engine.NewDelta(),
		cells:   map[engine.CellKey][]int64{},
		mirror:  map[string]map[string]*engine.MirrorOp{},
		nextOID: map[int32]int{},
		nextPID: map[int32]int{},
	}
	return w
}

// commit sends a transaction to the engine. Each change was already applied
// to the reference as it was drawn, so later changes in the same transaction
// see earlier ones, as they would in Postgres.
func (w *world) commit(t *rapid.T, changes []engine.Change) {
	if err := w.eng.Apply(&engine.Txn{Changes: changes}, w.delta); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

// flush mirrors what internal/flush does in one AGG transaction.
func (w *world) flush(t *rapid.T) {
	d := w.delta
	w.delta = engine.NewDelta()
	for table, ops := range d.Mirror {
		m := w.mirror[table]
		if m == nil {
			m = map[string]*engine.MirrorOp{}
			w.mirror[table] = m
		}
		for k, op := range ops {
			if op.Row == nil {
				delete(m, k)
			} else {
				m[k] = op
			}
		}
	}
	skip := map[int32]map[int16]bool{}
	for tenant, tables := range d.DimRebuild {
		for table := range tables {
			for _, r := range engine.AffectedRollups(w.cfg, table) {
				if skip[tenant] == nil {
					skip[tenant] = map[int16]bool{}
				}
				skip[tenant][r.ID] = true
			}
		}
	}
	for k, v := range d.NonZeroCells() {
		if skip[k.Tenant][k.Rollup] {
			continue
		}
		cur := w.cells[k]
		if cur == nil {
			cur = make([]int64, len(v))
			w.cells[k] = cur
		}
		for i := range v {
			cur[i] += v[i]
		}
		if cur[0] < 0 {
			t.Fatalf("cell %+v went negative: %v (a retraction without a matching contribution)", k, cur)
		}
	}
	for tenant, rs := range skip {
		var rollups []*config.Rollup
		for id := range rs {
			rollups = append(rollups, w.cfg.RollupByID(id))
			for k := range w.cells {
				if k.Tenant == tenant && k.Rollup == id {
					delete(w.cells, k)
				}
			}
		}
		var facts []*engine.Fact
		for _, f := range w.facts() {
			if f.Tenant == tenant {
				facts = append(facts, f)
			}
		}
		for k, v := range w.eng.CellsFrom(rollups, facts) {
			w.cells[k] = v
		}
	}
	for k, v := range w.cells {
		zero := true
		for _, x := range v {
			zero = zero && x == 0
		}
		if zero {
			delete(w.cells, k)
		}
	}
}

func (w *world) facts() []*engine.Fact {
	src := w.cfg.Sources.Get("orders")
	var out []*engine.Fact
	for _, op := range w.mirror["orders"] {
		f, err := w.eng.Resolve(src, op.Row)
		if err != nil {
			panic(err)
		}
		out = append(out, f)
	}
	return out
}

func ts(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04:05.999999-07:00") }

func drawTime(t *rapid.T, label string) time.Time {
	// Up to 160 days back: crosses hour retention (35d, 120d), top_customer's
	// day retention (100d), month and year boundaries, and a little future.
	secs := rapid.Int64Range(-160*86400, 3600).Draw(t, label)
	return fixedNow.Add(time.Duration(secs) * time.Second)
}

func (w *world) orderRow(t *rapid.T, tenant int32, id int) engine.Row {
	r := engine.Row{
		"tenant_id":      strconv.Itoa(int(tenant)),
		"order_id":       strconv.Itoa(id),
		"customer_id":    strconv.Itoa(rapid.IntRange(1, 5).Draw(t, "customer")),
		"product_id":     strconv.Itoa(rapid.IntRange(1, w.nextPID[tenant]).Draw(t, "product")),
		"quantity":       strconv.Itoa(rapid.IntRange(1, 4).Draw(t, "qty")),
		"amount_paise":   strconv.Itoa(rapid.IntRange(0, 500000).Draw(t, "amount")),
		"status":         rapid.SampledFrom(statuses).Draw(t, "status"),
		"payment_method": rapid.SampledFrom(payments).Draw(t, "pay"),
		"channel":        rapid.SampledFrom(channels).Draw(t, "channel"),
	}
	c := drawTime(t, "created")
	r["created_at"], r["updated_at"] = ts(c), ts(c)
	return r
}

func copyRow(r engine.Row) engine.Row {
	out := engine.Row{}
	for k, v := range r {
		out[k] = v
	}
	return out
}

func (w *world) init(t *rapid.T) {
	var changes []engine.Change
	for tenant := int32(1); tenant <= 3; tenant++ {
		changes = append(changes, engine.Change{Table: "tenants", Kind: engine.Insert, New: engine.Row{"tenant_id": strconv.Itoa(int(tenant)), "company_id": "1", "name": fmt.Sprintf("Store %d", tenant)}})
		for p := 1; p <= 4; p++ {
			changes = append(changes, engine.Change{Table: "products", Kind: engine.Insert, New: engine.Row{
				"tenant_id": strconv.Itoa(int(tenant)), "product_id": strconv.Itoa(p), "name": fmt.Sprintf("Product %d", p),
				"category": rapid.SampledFrom(cats).Draw(t, "cat"), "price_paise": "10000"}})
		}
		w.nextPID[tenant] = 4
		for c := 1; c <= 5; c++ {
			changes = append(changes, engine.Change{Table: "customers", Kind: engine.Insert, New: engine.Row{
				"tenant_id": strconv.Itoa(int(tenant)), "customer_id": strconv.Itoa(c), "full_name": "Test Customer",
				"email": "c@example.invalid", "city": rapid.SampledFrom(cities).Draw(t, "city"), "region": "west"}})
		}
	}
	for _, c := range changes {
		w.ref.Apply(c)
	}
	w.commit(t, changes)
}

func (w *world) randomOrder(t *rapid.T) engine.Row {
	rows := w.ref.Tables["orders"]
	if len(rows) == 0 {
		return nil
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return rows[rapid.SampledFrom(keys).Draw(t, "order")]
}

// change draws one random change for a transaction.
func (w *world) change(t *rapid.T) []engine.Change {
	tenant := int32(rapid.IntRange(1, 3).Draw(t, "tenant"))
	switch rapid.IntRange(0, 9).Draw(t, "op") {
	case 0, 1, 2, 3:
		w.nextOID[tenant]++
		return []engine.Change{{Table: "orders", Kind: engine.Insert, New: w.orderRow(t, tenant, w.nextOID[tenant])}}
	case 4, 5, 6:
		old := w.randomOrder(t)
		if old == nil {
			return nil
		}
		nw := copyRow(old)
		switch rapid.IntRange(0, 6).Draw(t, "field") {
		case 0, 1:
			nw["status"] = rapid.SampledFrom(statuses).Draw(t, "status")
		case 2:
			nw["amount_paise"] = strconv.Itoa(rapid.IntRange(0, 500000).Draw(t, "amount"))
		case 3:
			nw["created_at"] = ts(drawTime(t, "created")) // backdated or moved
		case 4:
			ot, _ := strconv.Atoi(old["tenant_id"])
			nw["product_id"] = strconv.Itoa(rapid.IntRange(1, w.nextPID[int32(ot)]).Draw(t, "product"))
		case 5:
			nw["customer_id"] = strconv.Itoa(rapid.IntRange(1, 5).Draw(t, "customer"))
		case 6:
			// Tenant reassignment: a fresh order id avoids colliding keys.
			w.nextOID[tenant]++
			nw["tenant_id"] = strconv.Itoa(int(tenant))
			nw["order_id"] = strconv.Itoa(w.nextOID[tenant] + 100000)
		}
		return []engine.Change{{Table: "orders", Kind: engine.Update, Old: old, New: nw}}
	case 7:
		old := w.randomOrder(t)
		if old == nil {
			return nil
		}
		return []engine.Change{{Table: "orders", Kind: engine.Delete, Old: old}}
	case 8:
		old := w.ref.Get("products", strconv.Itoa(int(tenant)), strconv.Itoa(rapid.IntRange(1, w.nextPID[tenant]).Draw(t, "product")))
		if old == nil {
			return nil
		}
		nw := copyRow(old)
		if rapid.Bool().Draw(t, "rename") {
			nw["name"] = old["name"] + "*"
		} else {
			nw["category"] = rapid.SampledFrom(cats).Draw(t, "cat")
		}
		return []engine.Change{{Table: "products", Kind: engine.Update, Old: old, New: nw}}
	default:
		old := w.ref.Get("customers", strconv.Itoa(int(tenant)), strconv.Itoa(rapid.IntRange(1, 5).Draw(t, "customer")))
		nw := copyRow(old)
		nw["city"] = rapid.SampledFrom(cities).Draw(t, "city")
		return []engine.Change{{Table: "customers", Kind: engine.Update, Old: old, New: nw}}
	}
}

func (w *world) drawQuery(t *rapid.T) *planner.Query {
	q := &planner.Query{Source: "orders", Filters: map[string]string{}}
	switch rapid.IntRange(0, 2).Draw(t, "scope") {
	case 0, 1:
		q.Tenants = []int32{int32(rapid.IntRange(1, 3).Draw(t, "qtenant"))}
	default:
		q.Tenants = []int32{1, 3} // a company with two stores
	}
	if rapid.Bool().Draw(t, "preset") {
		q.From, q.To, _ = timeutil.ResolvePreset(rapid.SampledFrom(timeutil.Presets).Draw(t, "range"), fixedNow, loc)
	} else {
		a, b := drawTime(t, "from"), drawTime(t, "to")
		if b.Before(a) {
			a, b = b, a
		}
		q.From, q.To = a, b
	}
	groups := [][]string{nil, {"status"}, {"category"}, {"product_id"}, {"customer_id"}, {"city"}, {"channel"}, {"payment_method"}}
	q.GroupBy = rapid.SampledFrom(groups).Draw(t, "group")
	switch rapid.IntRange(0, 3).Draw(t, "filter") {
	case 1:
		q.Filters["status"] = rapid.SampledFrom(statuses).Draw(t, "fstatus")
	case 2:
		q.Filters["category"] = rapid.SampledFrom(cats).Draw(t, "fcat")
	}
	q.Interval = rapid.SampledFrom([]timeutil.Grain{timeutil.GrainNone, timeutil.GrainNone, timeutil.GrainHour, timeutil.GrainDay, timeutil.GrainMonth}).Draw(t, "interval")
	return q
}

func (w *world) check(t *rapid.T) {
	p := &planner.Planner{Cfg: w.cfg, Loc: loc, Now: func() time.Time { return fixedNow }}
	q := w.drawQuery(t)
	plan, err := p.Plan(q)
	var np *planner.NotPreaggregated
	if errors.As(err, &np) {
		return // e.g. payment_method has no rollup in the base config
	}
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := plan.EvalMemory(w.cells, w.facts(), loc)
	want := w.ref.Answer(q)
	if diff := compare(got, want); diff != "" {
		t.Fatalf("query %+v (rollup %v, segments %v, edges %v):\n%s", *q, plan.Rollup.Name, plan.Segments, plan.Edges, diff)
	}
}

func compare(got, want planner.Result) string {
	var b strings.Builder
	for k, w := range want {
		if g := got[k]; fmt.Sprint(g) != fmt.Sprint(w) {
			fmt.Fprintf(&b, "  %+v: got %v want %v\n", k, g, w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(&b, "  %+v: got %v want nothing\n", k, g)
		}
	}
	return b.String()
}

// TestModel drives random change sequences through the engine and checks
// every query type on random ranges against the brute-force reference.
func TestModel(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		w := newWorld(t)
		w.init(rt)
		w.flush(rt)
		steps := rapid.IntRange(1, 60).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			var changes []engine.Change
			for n := rapid.IntRange(1, 4).Draw(rt, "txnSize"); n > 0; n-- {
				for _, c := range w.change(rt) {
					w.ref.Apply(c)
					changes = append(changes, c)
				}
			}
			if len(changes) > 0 {
				w.commit(rt, changes)
			}
			if rapid.IntRange(0, 2).Draw(rt, "flushNow") == 0 {
				continue // several transactions coalesce into one flush
			}
			w.flush(rt)
			for c := rapid.IntRange(1, 4).Draw(rt, "checks"); c > 0; c-- {
				w.check(rt)
			}
		}
		w.flush(rt)
		w.check(rt)
		w.checkRebuildEqualsIncremental(rt)
	})
}

// checkRebuildEqualsIncremental: recomputing every cell from the mirror gives
// exactly the incrementally maintained cells.
func (w *world) checkRebuildEqualsIncremental(t *rapid.T) {
	rebuilt := w.eng.CellsFrom(w.cfg.Rollups.Values(), w.facts())
	if len(rebuilt) != len(w.cells) {
		t.Fatalf("rebuild has %d cells, incremental has %d", len(rebuilt), len(w.cells))
	}
	for k, v := range rebuilt {
		if fmt.Sprint(w.cells[k]) != fmt.Sprint(v) {
			t.Fatalf("cell %+v: rebuild %v, incremental %v", k, v, w.cells[k])
		}
	}
}
