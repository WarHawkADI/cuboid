// Command storesim is the store platform's own application: it runs the
// normal reads and writes against the main database as the app role, and
// measures their latency. That latency is the PRD's "normal app query time",
// the number the database-safety target is judged on.
//
// It also serves a small app API (FR-6 by HTTP) and scenario controls.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"cuboid/internal/monitor"
)

type tenant struct {
	id        int32
	weight    float64
	nextOrder atomic.Int64
	products  int64
	customers int64
	mu        sync.Mutex
	recent    []int64 // recently placed orders, to advance through the lifecycle
}

type sim struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	tenants []*tenant
	cdf     []float64

	readsPS  atomic.Int64
	writesPS atomic.Int64
	paused   atomic.Bool
	surge    atomic.Pointer[surgeState]

	mu      sync.Mutex
	samples []sample // last 30 s of operations
	ops     atomic.Int64
	errs    atomic.Int64
}

type sample struct {
	at    time.Time
	ms    float64
	write bool
}

type surgeState struct {
	tenant int32
	until  time.Time
	factor int
}

var paymentsList = []string{"upi", "upi", "upi", "upi", "upi", "card", "card", "cod", "netbanking", "wallet"}
var channelsList = []string{"app", "app", "app", "web", "web", "marketplace"}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := pgxpool.ParseConfig(env("STORESIM_MAIN_DSN", "postgres://app:app@localhost:5433/shop"))
	if err != nil {
		log.Error("dsn", "err", err)
		os.Exit(1)
	}
	cfg.MaxConns = 24
	cfg.ConnConfig.RuntimeParams["application_name"] = "storesim"
	s := &sim{log: log}
	for i := 0; ; i++ {
		if s.pool, err = pgxpool.NewWithConfig(ctx, cfg); err == nil && s.pool.Ping(ctx) == nil {
			break
		}
		if i > 120 || ctx.Err() != nil {
			log.Error("connect", "err", err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
	reads, _ := strconv.Atoi(env("STORESIM_READS_PER_S", "140"))
	writes, _ := strconv.Atoi(env("STORESIM_WRITES_PER_S", "60"))
	s.readsPS.Store(int64(reads))
	s.writesPS.Store(int64(writes))
	go s.loadTenantsLoop(ctx)
	go s.run(ctx, false)
	go s.run(ctx, true)
	srv := &http.Server{Addr: env("STORESIM_LISTEN", ":8081"), Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("store app serving", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	<-ctx.Done()
	srv.Shutdown(context.Background())
}

// loadTenantsLoop reads store sizes once there is data, and again after a reseed.
func (s *sim) loadTenantsLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.loadTenants(ctx); err != nil {
			s.log.Info("waiting for seeded data", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Minute):
		}
	}
}

func (s *sim) loadTenants(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT t.tenant_id,
			coalesce((SELECT max(order_id) FROM orders o WHERE o.tenant_id = t.tenant_id), 0),
			coalesce((SELECT max(product_id) FROM products p WHERE p.tenant_id = t.tenant_id), 0),
			coalesce((SELECT max(customer_id) FROM customers c WHERE c.tenant_id = t.tenant_id), 0)
		FROM tenants t ORDER BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ts []*tenant
	var total float64
	for rows.Next() {
		t := &tenant{}
		var maxOrder int64
		if err := rows.Scan(&t.id, &maxOrder, &t.products, &t.customers); err != nil {
			return err
		}
		if t.products == 0 || t.customers == 0 {
			continue
		}
		t.nextOrder.Store(maxOrder)
		t.weight = float64(maxOrder) + 1000
		total += t.weight
		ts = append(ts, t)
	}
	if len(ts) == 0 {
		return errors.New("no stores yet")
	}
	cdf := make([]float64, len(ts))
	acc := 0.0
	for i, t := range ts {
		acc += t.weight / total
		cdf[i] = acc
	}
	s.mu.Lock()
	s.tenants, s.cdf = ts, cdf
	s.mu.Unlock()
	return nil
}

func (s *sim) pick() *tenant {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tenants) == 0 {
		return nil
	}
	if sg := s.surge.Load(); sg != nil && time.Now().Before(sg.until) && rand.IntN(sg.factor) > 0 {
		for _, t := range s.tenants {
			if t.id == sg.tenant {
				return t
			}
		}
	}
	x := rand.Float64()
	for i, c := range s.cdf {
		if x <= c {
			return s.tenants[i]
		}
	}
	return s.tenants[len(s.tenants)-1]
}

// run issues reads (or writes) at the configured rate, each in its own
// goroutine, Poisson-spaced.
func (s *sim) run(ctx context.Context, writes bool) {
	sem := make(chan struct{}, 64)
	for ctx.Err() == nil {
		rate := s.readsPS.Load()
		if writes {
			rate = s.writesPS.Load()
		}
		if s.paused.Load() || rate <= 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		time.Sleep(time.Duration(rand.ExpFloat64() / float64(rate) * float64(time.Second)))
		t := s.pick()
		if t == nil {
			time.Sleep(time.Second)
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			continue // the app is saturated; skip rather than queue unboundedly
		}
		go func() {
			defer func() { <-sem }()
			start := time.Now()
			var err error
			if writes {
				err = s.write(ctx, t)
			} else {
				err = s.read(ctx, t)
			}
			s.observe(time.Since(start), writes, err)
		}()
	}
}

func (s *sim) observe(d time.Duration, write bool, err error) {
	s.ops.Add(1)
	if err != nil {
		s.errs.Add(1)
		return
	}
	s.mu.Lock()
	now := time.Now()
	s.samples = append(s.samples, sample{at: now, ms: float64(d.Microseconds()) / 1000, write: write})
	cut := 0
	for cut < len(s.samples) && now.Sub(s.samples[cut].at) > 30*time.Second {
		cut++
	}
	if cut > 0 {
		s.samples = append([]sample(nil), s.samples[cut:]...)
	}
	s.mu.Unlock()
}

func (s *sim) read(ctx context.Context, t *tenant) error {
	max := t.nextOrder.Load()
	if max < 1 {
		return nil
	}
	switch rand.IntN(3) {
	case 0: // get an order by id
		var amount int64
		err := s.pool.QueryRow(ctx, "SELECT amount_paise FROM orders WHERE tenant_id = $1 AND order_id = $2", t.id, 1+rand.Int64N(max)).Scan(&amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	case 1: // a customer's last 10 orders
		rows, err := s.pool.Query(ctx, `SELECT order_id, status, amount_paise, created_at FROM orders
			WHERE tenant_id = $1 AND customer_id = $2 ORDER BY created_at DESC LIMIT 10`, t.id, 1+rand.Int64N(t.customers))
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		rows.Close()
		return rows.Err()
	default: // a product with its details
		var name string
		err := s.pool.QueryRow(ctx, "SELECT name FROM products WHERE tenant_id = $1 AND product_id = $2", t.id, 1+rand.Int64N(t.products)).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
}

func (s *sim) write(ctx context.Context, t *tenant) error {
	r := rand.IntN(1000)
	switch {
	case r < 800:
		_, err := s.placeOrder(ctx, t, orderInput{})
		return err
	case r < 900:
		return s.advance(ctx, t)
	case r < 940: // cancel a placed order
		if id, ok := t.recentOrder(); ok {
			_, err := s.pool.Exec(ctx, "UPDATE orders SET status = 'cancelled', updated_at = now() WHERE tenant_id = $1 AND order_id = $2 AND status = 'placed'", t.id, id)
			return err
		}
	case r < 960: // correct an amount
		if id, ok := t.recentOrder(); ok {
			_, err := s.pool.Exec(ctx, "UPDATE orders SET amount_paise = amount_paise + 1000, updated_at = now() WHERE tenant_id = $1 AND order_id = $2", t.id, id)
			return err
		}
	case r < 975: // return an old delivered order
		_, err := s.pool.Exec(ctx, `UPDATE orders SET status = 'returned', updated_at = now()
			WHERE tenant_id = $1 AND order_id = $2 AND status = 'delivered'`, t.id, 1+rand.Int64N(max64(1, t.nextOrder.Load()*9/10)))
		return err
	case r < 995: // a late update to an order weeks old
		_, err := s.pool.Exec(ctx, `UPDATE orders SET quantity = quantity + 1, amount_paise = amount_paise + amount_paise / quantity, updated_at = now()
			WHERE tenant_id = $1 AND order_id = $2`, t.id, 1+rand.Int64N(max64(1, t.nextOrder.Load()*95/100)))
		return err
	default: // a rare hard delete
		_, err := s.pool.Exec(ctx, "DELETE FROM orders WHERE tenant_id = $1 AND order_id = $2", t.id, 1+rand.Int64N(max64(1, t.nextOrder.Load())))
		return err
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (t *tenant) recentOrder() (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.recent) == 0 {
		return 0, false
	}
	return t.recent[rand.IntN(len(t.recent))], true
}

func (s *sim) advance(ctx context.Context, t *tenant) error {
	id, ok := t.recentOrder()
	if !ok {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE orders SET status = CASE status WHEN 'placed' THEN 'shipped' WHEN 'shipped' THEN 'delivered' ELSE status END,
		updated_at = now() WHERE tenant_id = $1 AND order_id = $2 AND status IN ('placed', 'shipped')`, t.id, id)
	return err
}

type orderInput struct {
	CustomerID int64  `json:"customer_id"`
	ProductID  int64  `json:"product_id"`
	Quantity   int32  `json:"quantity"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"` // optional, to insert a backdated order
	Channel    string `json:"channel"`
	Payment    string `json:"payment_method"`
}

type orderOut struct {
	TenantID    int32     `json:"tenant_id"`
	OrderID     int64     `json:"order_id"`
	CustomerID  int64     `json:"customer_id"`
	ProductID   int64     `json:"product_id"`
	Quantity    int32     `json:"quantity"`
	AmountPaise int64     `json:"amount_paise"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *sim) placeOrder(ctx context.Context, t *tenant, in orderInput) (*orderOut, error) {
	o := &orderOut{TenantID: t.id, CustomerID: in.CustomerID, ProductID: in.ProductID, Quantity: in.Quantity, Status: in.Status, CreatedAt: time.Now()}
	if o.CustomerID == 0 {
		o.CustomerID = 1 + rand.Int64N(t.customers)
	}
	if o.ProductID == 0 {
		o.ProductID = 1 + rand.Int64N(min(t.products, 1+int64(rand.ExpFloat64()*float64(t.products)/4)))
	}
	if o.Quantity == 0 {
		o.Quantity = 1
	}
	if o.Status == "" {
		o.Status = "placed"
	}
	if in.CreatedAt != "" {
		c, err := time.Parse(time.RFC3339, in.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("created_at must be RFC 3339, like 2026-10-01T12:00:00+05:30")
		}
		o.CreatedAt = c
	}
	pay, ch := in.Payment, in.Channel
	if pay == "" {
		pay = paymentsList[rand.IntN(len(paymentsList))]
	}
	if ch == "" {
		ch = channelsList[rand.IntN(len(channelsList))]
	}
	for attempt := 0; attempt < 5; attempt++ {
		o.OrderID = t.nextOrder.Add(1)
		err := s.pool.QueryRow(ctx, `INSERT INTO orders (tenant_id, order_id, customer_id, product_id, quantity, amount_paise, status, payment_method, channel, created_at, updated_at)
			SELECT $1, $2, $3, $4, $5, p.price_paise * $5, $6, $7, $8, $9, $9 FROM products p WHERE p.tenant_id = $1 AND p.product_id = $4
			ON CONFLICT DO NOTHING RETURNING amount_paise`,
			t.id, o.OrderID, o.CustomerID, o.ProductID, o.Quantity, o.Status, pay, ch, o.CreatedAt).Scan(&o.AmountPaise)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the id was taken (for example by an insert from psql); take the next one
		}
		if err != nil {
			return nil, err
		}
		if o.Status == "placed" {
			t.mu.Lock()
			t.recent = append(t.recent, o.OrderID)
			if len(t.recent) > 2000 {
				t.recent = t.recent[len(t.recent)-2000:]
			}
			t.mu.Unlock()
		}
		return o, nil
	}
	return nil, errors.New("could not find a free order id")
}

func (s *sim) tenantByID(id int32) *tenant {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tenants {
		if t.id == id {
			return t
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *sim) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics/window", func(w http.ResponseWriter, r *http.Request) {
		secs, _ := strconv.Atoi(r.URL.Query().Get("seconds"))
		if secs <= 0 || secs > 30 {
			secs = 3
		}
		writeJSON(w, 200, s.window(time.Duration(secs)*time.Second))
	})
	mux.HandleFunc("GET /control/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"reads_per_s": s.readsPS.Load(), "writes_per_s": s.writesPS.Load(), "paused": s.paused.Load(), "ops": s.ops.Load(), "errors": s.errs.Load()})
	})
	mux.HandleFunc("POST /control/rates", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Reads  *int64 `json:"reads_per_s"`
			Writes *int64 `json:"writes_per_s"`
			Paused *bool  `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if b.Reads != nil {
			s.readsPS.Store(*b.Reads)
		}
		if b.Writes != nil {
			s.writesPS.Store(*b.Writes)
		}
		if b.Paused != nil {
			s.paused.Store(*b.Paused)
		}
		writeJSON(w, 200, map[string]any{"reads_per_s": s.readsPS.Load(), "writes_per_s": s.writesPS.Load(), "paused": s.paused.Load()})
	})
	mux.HandleFunc("POST /control/scenario", s.scenario)
	mux.HandleFunc("POST /app/tenants/{id}/orders", func(w http.ResponseWriter, r *http.Request) {
		t := s.pathTenant(w, r)
		if t == nil {
			return
		}
		var in orderInput
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		start := time.Now()
		o, err := s.placeOrder(r.Context(), t, in)
		s.observe(time.Since(start), true, err)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, o)
	})
	mux.HandleFunc("PATCH /app/tenants/{id}/orders/{order}", func(w http.ResponseWriter, r *http.Request) {
		t := s.pathTenant(w, r)
		if t == nil {
			return
		}
		oid, _ := strconv.ParseInt(r.PathValue("order"), 10, 64)
		var b struct {
			Status      *string `json:"status"`
			AmountPaise *int64  `json:"amount_paise"`
			CreatedAt   *string `json:"created_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		var created *time.Time
		if b.CreatedAt != nil {
			c, err := time.Parse(time.RFC3339, *b.CreatedAt)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": "created_at must be RFC 3339"})
				return
			}
			created = &c
		}
		var out orderOut
		err := s.pool.QueryRow(r.Context(), `UPDATE orders SET status = coalesce($3, status), amount_paise = coalesce($4, amount_paise),
				created_at = coalesce($5, created_at), updated_at = now()
			WHERE tenant_id = $1 AND order_id = $2
			RETURNING tenant_id, order_id, customer_id, product_id, quantity, amount_paise, status, created_at`,
			t.id, oid, b.Status, b.AmountPaise, created).Scan(&out.TenantID, &out.OrderID, &out.CustomerID, &out.ProductID, &out.Quantity, &out.AmountPaise, &out.Status, &out.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, 404, map[string]string{"error": "no such order"})
			return
		}
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("DELETE /app/tenants/{id}/orders/{order}", func(w http.ResponseWriter, r *http.Request) {
		t := s.pathTenant(w, r)
		if t == nil {
			return
		}
		oid, _ := strconv.ParseInt(r.PathValue("order"), 10, 64)
		tag, err := s.pool.Exec(r.Context(), "DELETE FROM orders WHERE tenant_id = $1 AND order_id = $2", t.id, oid)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]int64{"deleted": tag.RowsAffected()})
	})
	return mux
}

func (s *sim) pathTenant(w http.ResponseWriter, r *http.Request) *tenant {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	t := s.tenantByID(int32(id))
	if err != nil || t == nil {
		writeJSON(w, 404, map[string]string{"error": "no such store"})
		return nil
	}
	return t
}

// window summarises the app's operations over the last d.
func (s *sim) window(d time.Duration) map[string]any {
	s.mu.Lock()
	cut := time.Now().Add(-d)
	var all, reads, writes []float64
	for _, x := range s.samples {
		if x.at.Before(cut) {
			continue
		}
		all = append(all, x.ms)
		if x.write {
			writes = append(writes, x.ms)
		} else {
			reads = append(reads, x.ms)
		}
	}
	s.mu.Unlock()
	a := monitor.Summarise("app", all)
	rd := monitor.Summarise("reads", reads)
	wr := monitor.Summarise("writes", writes)
	return map[string]any{"p50_ms": a.P50, "p95_ms": a.P95, "p99_ms": a.P99, "ops_per_s": float64(a.Count) / d.Seconds(),
		"reads": rd, "writes": wr, "window_s": d.Seconds()}
}

func (s *sim) scenario(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name   string `json:"name"`
		Tenant int32  `json:"tenant"`
		N      int    `json:"n"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	if b.Tenant == 0 {
		b.Tenant = 42
	}
	t := s.tenantByID(b.Tenant)
	if t == nil {
		writeJSON(w, 404, map[string]string{"error": "no such store"})
		return
	}
	ctx := r.Context()
	start := time.Now()
	switch b.Name {
	case "bulk_update":
		if b.N <= 0 {
			b.N = 50000
		}
		// One transaction correcting 50,000 orders' amounts: every contribution
		// must be retracted and re-added, atomically.
		tag, err := s.pool.Exec(ctx, `UPDATE orders SET amount_paise = amount_paise + 100, updated_at = now()
			WHERE tenant_id = $1 AND order_id > $2`, t.id, max64(0, t.nextOrder.Load()-int64(b.N)))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"scenario": b.Name, "rows": tag.RowsAffected(), "took_ms": time.Since(start).Milliseconds()})
	case "surge":
		s.surge.Store(&surgeState{tenant: t.id, until: time.Now().Add(30 * time.Second), factor: 10})
		writeJSON(w, 200, map[string]any{"scenario": b.Name, "tenant": t.id, "seconds": 30})
	case "late_updates":
		if b.N <= 0 {
			b.N = 500
		}
		var total int64
		for i := 0; i < b.N; i++ {
			old := max64(1, t.nextOrder.Load()*int64(70+rand.IntN(20))/100)
			tag, err := s.pool.Exec(ctx, "UPDATE orders SET status = 'returned', updated_at = now() WHERE tenant_id = $1 AND order_id = $2 AND status = 'delivered'", t.id, old)
			if err == nil {
				total += tag.RowsAffected()
			}
		}
		writeJSON(w, 200, map[string]any{"scenario": b.Name, "rows": total, "took_ms": time.Since(start).Milliseconds()})
	case "truncate_reload":
		// TRUNCATE invalidates every rollup; Cuboid must notice and rebuild.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		defer tx.Rollback(context.Background())
		steps := []string{"CREATE TEMP TABLE keep_orders ON COMMIT DROP AS SELECT * FROM orders", "TRUNCATE orders", "INSERT INTO orders SELECT * FROM keep_orders"}
		for _, st := range steps {
			if _, err := tx.Exec(ctx, st); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"scenario": b.Name, "took_ms": time.Since(start).Milliseconds()})
	default:
		writeJSON(w, 400, map[string]string{"error": "scenario is bulk_update, surge, late_updates or truncate_reload"})
	}
}
