// Command seed generates the fallback dataset (Section 7.2 of the build
// prompt) and loads it with parallel COPY: the registry of stores and
// companies into the platform database, and each store's orders, products,
// customers and alerts into that store's own database (ADR-012), created
// from store_template and pinned to the store with CHECK constraints.
//
//	seed --tier dev|small|main [--seed 42] [--anchor 2026-10-31T12:00:00+05:30]
//
// The same seed and anchor produce identical data. Records are orders;
// customers, products and SOC alerts scale with them.
package main

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // the runtime image has no zone database; Asia/Kolkata must load anyway

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tierSpec struct {
	orders int
	stores []int32
	zipf   float64
}

func tiers(name string) (tierSpec, error) {
	span := func(a, b int32) []int32 {
		var out []int32
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
		return out
	}
	switch name {
	case "dev":
		return tierSpec{orders: 100_000, stores: span(40, 44), zipf: 0.8}, nil
	case "small":
		return tierSpec{orders: 1_000_000, stores: span(37, 46), zipf: 0.8}, nil
	case "main":
		return tierSpec{orders: 10_000_000, stores: span(1, 100), zipf: 1.0}, nil
	}
	return tierSpec{}, fmt.Errorf("unknown tier %q (use dev, small or main)", name)
}

var loc, _ = time.LoadLocation("Asia/Kolkata")

func main() {
	tierName := flag.String("tier", envOr("SEED_TIER", "small"), "dev (100k orders, 5 stores) | small (1M, 10) | main (10M, 100)")
	seed := flag.Uint64("seed", 42, "random seed")
	dsn := flag.String("dsn", envOr("SEED_MAIN_DSN", "postgres://postgres:postgres@localhost:5433/shop"), "main database (superuser: truncates, loads and indexes)")
	anchorS := flag.String("anchor", "", "end of the generated history (default: the start of the current hour)")
	analytics := flag.Bool("analytics-indexes", true, "create the fair analytics indexes the direct baseline needs")
	workers := flag.Int("workers", 6, "parallel loaders")
	hash := flag.Bool("hash", false, "print a hash of every order row (determinism check; use on the dev tier)")
	flag.Parse()

	t, err := tiers(*tierName)
	if err != nil {
		log.Fatal(err)
	}
	anchor := time.Now().In(loc).Truncate(time.Hour)
	if *anchorS != "" {
		if anchor, err = time.Parse(time.RFC3339, *anchorS); err != nil {
			log.Fatalf("anchor: %v", err)
		}
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(*dsn)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = int32(*workers + 2)
	cfg.ConnConfig.RuntimeParams["application_name"] = "cuboid-seed"
	var pool *pgxpool.Pool
	for i := 0; ; i++ {
		if pool, err = pgxpool.NewWithConfig(ctx, cfg); err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		if i > 60 {
			log.Fatalf("connect to the main database: %v", err)
		}
		time.Sleep(time.Second)
	}
	defer pool.Close()

	if *hash {
		ids, err := storeIDs(ctx, pool)
		if err != nil {
			log.Fatal(err)
		}
		var parts []string
		for _, id := range ids {
			sp, err := storePool(ctx, *dsn, id, 1)
			if err != nil {
				log.Fatal(err)
			}
			var h string
			if err := sp.QueryRow(ctx, `SELECT coalesce(md5(string_agg(o::text, '|' ORDER BY tenant_id, order_id)), '') FROM orders o`).Scan(&h); err != nil {
				log.Fatal(err)
			}
			sp.Close()
			parts = append(parts, h)
		}
		fmt.Printf("%x\n", md5.Sum([]byte(strings.Join(parts, "|"))))
		return
	}

	start := time.Now()
	plans := planStores(t, *seed)
	fmt.Printf("Seeding tier %s: %d orders across %d stores, history to %s (seed %d)\n", *tierName, t.orders, len(plans), anchor.Format(time.RFC3339), *seed)
	must(announceLoad(ctx, pool, true))
	must(prepare(ctx, pool))
	must(insertStores(ctx, pool, plans))
	curve := newCurve(anchor)

	work := make(chan *storePlan)
	var wg sync.WaitGroup
	var done atomic.Int64
	var loaded atomic.Int64
	errs := make(chan error, len(plans))
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				n, err := seedStore(ctx, pool, *dsn, p, curve, anchor, *seed, *analytics)
				if err != nil {
					errs <- fmt.Errorf("store %d: %w", p.id, err)
					return
				}
				loaded.Add(n)
				fmt.Printf("  store %3d  %9d orders  (%d/%d stores, %s)\n", p.id, p.orders, done.Add(1), len(plans), time.Since(start).Round(time.Second))
			}
		}()
	}
	sorted := append([]*storePlan(nil), plans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].orders > sorted[j].orders })
	for _, p := range sorted {
		work <- p
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		log.Fatal(err)
	}
	fmt.Printf("Loaded %d rows into %d store databases in %s\n", loaded.Load(), len(plans), time.Since(start).Round(time.Second))
	must(announceLoad(ctx, pool, false))
	fmt.Printf("Done in %s\n", time.Since(start).Round(time.Second))
	summary(ctx, pool, *dsn)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

type storePlan struct {
	id       int32
	company  int32
	name     string
	orders   int
	business business
}

// planStores sizes stores with a Zipf-like skew ("a few tenants are much
// bigger than the rest") and groups them into companies. Store 42 is the
// largest and shares company 7 with store 43 and one more store.
func planStores(t tierSpec, seed uint64) []*storePlan {
	rng := rand.New(rand.NewPCG(seed, 7))
	ids := append([]int32(nil), t.stores...)
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	// Ranks: 42 first, 43 sixth (a mid-large store), the rest shuffled.
	var rest []int32
	for _, id := range ids {
		if id != 42 && id != 43 {
			rest = append(rest, id)
		}
	}
	order := append([]int32{42}, rest...)
	pos := 5
	if pos > len(order) {
		pos = len(order)
	}
	order = append(order[:pos], append([]int32{43}, order[pos:]...)...)
	weights := make([]float64, len(order))
	var sum float64
	for i := range order {
		weights[i] = 1 / math.Pow(float64(i+1), t.zipf)
		sum += weights[i]
	}
	minOrders := 5000
	if t.orders < 1_000_000 {
		minOrders = 1000
	}
	plans := make([]*storePlan, len(order))
	total := 0
	for i, id := range order {
		n := int(math.Round(float64(t.orders) * weights[i] / sum))
		if n < minOrders {
			n = minOrders
		}
		plans[i] = &storePlan{id: id, orders: n}
		total += n
	}
	plans[0].orders += t.orders - total // exact total; the largest store absorbs rounding
	// Companies: company 7 owns 42, 43 and one more; about a third of the
	// other companies own 2 to 5 stores.
	var third int32
	for _, p := range plans {
		if p.id != 42 && p.id != 43 {
			third = p.id
			break
		}
	}
	company := map[int32]int32{42: 7, 43: 7, third: 7}
	next := int32(1)
	var free []int32
	for _, id := range ids {
		if _, ok := company[id]; !ok {
			free = append(free, id)
		}
	}
	for len(free) > 0 {
		if next == 7 {
			next++
		}
		size := 1
		if rng.IntN(3) == 0 {
			size = 2 + rng.IntN(4)
		}
		if size > len(free) {
			size = len(free)
		}
		for _, id := range free[:size] {
			company[id] = next
		}
		free = free[size:]
		next++
	}
	for _, p := range plans {
		p.company = company[p.id]
		p.business = businesses[int(p.id*7)%len(businesses)]
		p.name = fmt.Sprintf("%s %s", storeWords[int(p.id*3)%len(storeWords)], titleCase(p.business.kind))
	}
	return plans
}

func titleCase(s string) string {
	switch s {
	case "home":
		return "Home Goods"
	case "fashion":
		return "Fashion"
	case "electronics":
		return "Electronics"
	case "grocery":
		return "Grocers"
	case "beauty":
		return "Beauty"
	}
	return "Books"
}

// announceLoad tells Cuboid and the store app a bulk load is running, and ends
// every replication session, so the load is not streamed row by row. When the
// flag clears, Cuboid finds no slots and loads a snapshot of each store.
func announceLoad(ctx context.Context, pool *pgxpool.Pool, loading bool) error {
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS seed_status (id int PRIMARY KEY DEFAULT 1 CHECK (id = 1), loading boolean NOT NULL DEFAULT false)",
		"INSERT INTO seed_status DEFAULT VALUES ON CONFLICT DO NOTHING",
		"GRANT SELECT ON seed_status TO cuboid_cdc, app",
		fmt.Sprintf("UPDATE seed_status SET loading = %v", loading),
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	if !loading {
		return nil
	}
	// Cuboid checks the flag every 2 seconds and may be creating a slot
	// meanwhile; a store's database cannot be dropped while a slot uses it.
	// Keep ending sessions and dropping slots until none has appeared for 3 s.
	quiet := 0
	for i := 0; i < 60 && quiet < 3; i++ {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'cuboid_%'").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			quiet++
		} else {
			quiet = 0
			pool.Exec(ctx, "SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name LIKE 'cuboid_%' AND active_pid IS NOT NULL")
			time.Sleep(300 * time.Millisecond)
			pool.Exec(ctx, "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name LIKE 'cuboid_%' AND NOT active")
		}
		time.Sleep(time.Second)
	}
	return nil
}

// prepare empties the registry and drops every store's database: the load
// creates them again from the template.
func prepare(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, "TRUNCATE tenants, companies"); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, "SELECT datname FROM pg_database WHERE datname ~ '^store_[0-9]+$' ORDER BY 1")
	if err != nil {
		return err
	}
	var dbs []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		dbs = append(dbs, n)
	}
	rows.Close()
	for _, db := range dbs {
		if _, err := pool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop %s: %w", db, err)
		}
	}
	if len(dbs) > 0 {
		fmt.Printf("Dropped %d store databases\n", len(dbs))
	}
	return nil
}

// storeDatabase is a store's own database (config/metrics.yaml, tenancy.databases).
func storeDatabase(id int32) string { return fmt.Sprintf("store_%d", id) }

// storePool connects to a store's database with the seed's role.
func storePool(ctx context.Context, dsn string, id int32, conns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Database = storeDatabase(id)
	cfg.MaxConns = conns
	cfg.ConnConfig.RuntimeParams["application_name"] = "cuboid-seed"
	return pgxpool.NewWithConfig(ctx, cfg)
}

func storeIDs(ctx context.Context, pool *pgxpool.Pool) ([]int32, error) {
	rows, err := pool.Query(ctx, "SELECT tenant_id FROM tenants ORDER BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int32
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// seedStore creates one store's database from the template, pins every table
// to that store, loads it and builds its indexes: a store from start to end,
// on one worker, while other workers do other stores.
func seedStore(ctx context.Context, platform *pgxpool.Pool, dsn string, p *storePlan, cv *curve, anchor time.Time, seed uint64, analytics bool) (int64, error) {
	db := pgx.Identifier{storeDatabase(p.id)}.Sanitize()
	var err error
	for i := 0; i < 30; i++ {
		// The template refuses to be copied while anyone is connected to it,
		// such as Cuboid reading its schema at startup: wait and retry.
		if _, err = platform.Exec(ctx, "CREATE DATABASE "+db+" TEMPLATE store_template"); err == nil || !strings.Contains(err.Error(), "being accessed by other users") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", db, err)
	}
	pool, err := storePool(ctx, dsn, p.id, 2)
	if err != nil {
		return 0, err
	}
	defer pool.Close()
	stmts := []string{
		// Separation at the source: no row of another store can be written here.
		fmt.Sprintf("ALTER TABLE orders ADD CONSTRAINT orders_one_store CHECK (tenant_id = %d)", p.id),
		fmt.Sprintf("ALTER TABLE products ADD CONSTRAINT products_one_store CHECK (tenant_id = %d)", p.id),
		fmt.Sprintf("ALTER TABLE customers ADD CONSTRAINT customers_one_store CHECK (tenant_id = %d)", p.id),
		fmt.Sprintf("ALTER TABLE soc_alerts ADD CONSTRAINT soc_alerts_one_store CHECK (tenant_id = %d)", p.id),
		// Loaded without them, built after: faster than maintaining them row by row.
		"ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_pkey",
		"ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_pkey",
		"ALTER TABLE soc_alerts DROP CONSTRAINT IF EXISTS soc_alerts_pkey",
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return 0, fmt.Errorf("%s: %w", s, err)
		}
	}
	n, err := loadStore(ctx, pool, p, cv, anchor, seed)
	if err != nil {
		return 0, err
	}
	return n, finishStore(ctx, pool, analytics)
}

func insertStores(ctx context.Context, pool *pgxpool.Pool, plans []*storePlan) error {
	seen := map[int32]bool{}
	for _, p := range plans {
		if !seen[p.company] {
			seen[p.company] = true
			if _, err := pool.Exec(ctx, "INSERT INTO companies (company_id, name) VALUES ($1, $2)", p.company, fmt.Sprintf("Company %d", p.company)); err != nil {
				return err
			}
		}
	}
	for _, p := range plans {
		if _, err := pool.Exec(ctx, "INSERT INTO tenants (tenant_id, company_id, name) VALUES ($1, $2, $3)", p.id, p.company, p.name); err != nil {
			return err
		}
	}
	return nil
}

// finishStore builds a store's keys and indexes in its own database.
func finishStore(ctx context.Context, pool *pgxpool.Pool, analytics bool) error {
	stmts := []string{
		"ALTER TABLE orders ADD PRIMARY KEY (tenant_id, order_id)",
		"ALTER TABLE customers ADD PRIMARY KEY (tenant_id, customer_id)",
		"ALTER TABLE soc_alerts ADD PRIMARY KEY (tenant_id, alert_id)",
		// The store app's own query: a customer's recent orders.
		"CREATE INDEX orders_customer_time ON orders (tenant_id, customer_id, created_at)",
	}
	if analytics {
		// Fair indexes for the direct baseline, so it is not a straw man.
		stmts = append(stmts,
			"CREATE INDEX orders_tenant_time ON orders (tenant_id, created_at)",
			"CREATE INDEX orders_tenant_status_time ON orders (tenant_id, status, created_at)",
			"CREATE INDEX orders_tenant_product_time ON orders (tenant_id, product_id, created_at)")
	}
	stmts = append(stmts, "ANALYZE")
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func summary(ctx context.Context, platform *pgxpool.Pool, dsn string) {
	ids, err := storeIDs(ctx, platform)
	if err != nil {
		return
	}
	var total, largest, smallest int64
	var largestID int32
	smallest = math.MaxInt64
	for _, id := range ids {
		pool, err := storePool(ctx, dsn, id, 1)
		if err != nil {
			continue
		}
		var c int64
		pool.QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&c)
		pool.Close()
		if c > largest {
			largest, largestID = c, id
		}
		if c < smallest {
			smallest = c
		}
		total += c
	}
	if total == 0 {
		return
	}
	fmt.Printf("Orders: %d across %d stores, each in its own database. Largest: store %d with %d (%.1f%%). Smallest: %d.\n",
		total, len(ids), largestID, largest, 100*float64(largest)/float64(total), smallest)
}

// curve is the order-time distribution over the 24 months before the anchor:
// growth, weekly and daily cycles, and a festive uplift in October-November.
type curve struct {
	start time.Time
	cdf   []float64
}

var hourly = []float64{0.25, 0.15, 0.10, 0.08, 0.08, 0.12, 0.25, 0.45, 0.70, 0.90, 1.00, 1.05,
	1.10, 1.05, 1.00, 1.00, 1.05, 1.15, 1.30, 1.45, 1.50, 1.35, 1.00, 0.55}

func newCurve(anchor time.Time) *curve {
	start := anchor.AddDate(-2, 0, 0)
	hours := int(anchor.Sub(start).Hours())
	c := &curve{start: start, cdf: make([]float64, hours)}
	var acc float64
	for h := 0; h < hours; h++ {
		t := start.Add(time.Duration(h) * time.Hour).In(loc)
		w := 0.45 + 0.55*float64(h)/float64(hours)
		switch t.Weekday() {
		case time.Saturday, time.Sunday:
			w *= 1.18
		case time.Monday:
			w *= 0.95
		}
		w *= hourly[t.Hour()]
		if (t.Month() == time.October && t.Day() >= 10) || (t.Month() == time.November && t.Day() <= 15) {
			w *= 1.7
		}
		if t.Month() == time.January && t.Day() >= 20 && t.Day() <= 26 {
			w *= 1.3
		}
		acc += w
		c.cdf[h] = acc
	}
	return c
}

func (c *curve) sample(rng *rand.Rand) time.Time {
	x := rng.Float64() * c.cdf[len(c.cdf)-1]
	h := sort.SearchFloat64s(c.cdf, x)
	return c.start.Add(time.Duration(h)*time.Hour + time.Duration(rng.Int64N(int64(time.Hour/time.Microsecond)))*time.Microsecond)
}

func weighted[T any](rng *rand.Rand, items []T, weight func(T) float64) T {
	x := rng.Float64()
	for _, it := range items {
		x -= weight(it)
		if x <= 0 {
			return it
		}
	}
	return items[len(items)-1]
}

type product struct {
	id    int64
	price int64
}

func loadStore(ctx context.Context, pool *pgxpool.Pool, p *storePlan, cv *curve, anchor time.Time, seed uint64) (int64, error) {
	rng := rand.New(rand.NewPCG(seed, uint64(p.id)))
	cats := append([]category(nil), p.business.categories...)
	nCat := 12 + rng.IntN(len(cats)-11)
	if nCat > len(cats) {
		nCat = len(cats)
	}
	cats = cats[:nCat]

	nProd := 200 + p.orders/600
	if nProd > 3000 {
		nProd = 3000
	}
	products := make([]product, nProd)
	var prodRows [][]any
	for i := range products {
		c := cats[int(math.Min(float64(nCat-1), math.Floor(rng.ExpFloat64()*float64(nCat)/2.5)))]
		price := int64(math.Round(c.price*math.Exp(rng.NormFloat64()*c.spread))/10)*1000 - 100 // ends in 9.00
		if price < 4900 {
			price = 4900
		}
		products[i] = product{id: int64(i + 1), price: price}
		name := fmt.Sprintf("%s %s, %s", adjectives[rng.IntN(len(adjectives))], c.nouns[rng.IntN(len(c.nouns))], colours[rng.IntN(len(colours))])
		prodRows = append(prodRows, []any{p.id, int64(i + 1), name, c.name, price})
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"products"}, []string{"tenant_id", "product_id", "name", "category", "price_paise"}, pgx.CopyFromRows(prodRows)); err != nil {
		return 0, err
	}
	// Product popularity: a long tail.
	pcdf := make([]float64, nProd)
	var acc float64
	for i := range pcdf {
		acc += 1 / math.Pow(float64(i+5), 0.9)
		pcdf[i] = acc
	}

	nCust := p.orders / 6
	if nCust < 50 {
		nCust = 50
	}
	ccdf := make([]float64, len(cities))
	acc = 0
	for i := range ccdf {
		acc += 1 / math.Pow(float64(i+2), 0.8)
		ccdf[i] = acc
	}
	custRows := 0
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"customers"}, []string{"tenant_id", "customer_id", "full_name", "email", "phone", "city", "region", "created_at"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if custRows >= nCust {
				return nil, nil
			}
			custRows++
			ci := cities[sort.SearchFloat64s(ccdf, rng.Float64()*ccdf[len(ccdf)-1])]
			id := int64(custRows)
			return []any{p.id, id, fmt.Sprintf("Test Customer %d", id), fmt.Sprintf("customer%d@store%d.example.invalid", id, p.id),
				fmt.Sprintf("+91-00000-%05d", id%100000), ci.name, ci.region, cv.start.Add(time.Duration(rng.Int64N(int64(anchor.Sub(cv.start)))))}, nil
		}))
	if err != nil {
		return 0, err
	}

	times := make([]int64, p.orders)
	for i := range times {
		times[i] = cv.sample(rng).UnixMicro()
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	i := 0
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"orders"},
		[]string{"tenant_id", "order_id", "customer_id", "product_id", "quantity", "amount_paise", "status", "payment_method", "channel", "created_at", "updated_at"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if i >= len(times) {
				return nil, nil
			}
			created := time.UnixMicro(times[i])
			i++
			var cust int64
			if rng.IntN(10) < 3 { // repeat buyers
				cust = 1 + rng.Int64N(int64(nCust)/5+1)
			} else {
				cust = 1 + rng.Int64N(int64(nCust))
			}
			pr := products[sort.SearchFloat64s(pcdf, rng.Float64()*pcdf[len(pcdf)-1])]
			qty := []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 3, 4}[rng.IntN(20)]
			status, updated := lifecycle(rng, created, anchor)
			pay := weighted(rng, payments, func(x struct {
				name   string
				weight float64
			}) float64 {
				return x.weight
			}).name
			ch := weighted(rng, channels, func(x struct {
				name   string
				weight float64
			}) float64 {
				return x.weight
			}).name
			return []any{p.id, int64(i), cust, pr.id, int32(qty), pr.price * int64(qty), status, pay, ch, created, updated}, nil
		}))
	if err != nil {
		return 0, err
	}
	alerts, err := loadAlerts(ctx, pool, p, rng, cv, anchor)
	return int64(len(times)+nCust+nProd) + alerts, err
}

// lifecycle picks a status consistent with the order's age: recent orders are
// mid-lifecycle, so the store app can advance them.
func lifecycle(rng *rand.Rand, created, anchor time.Time) (string, time.Time) {
	age := anchor.Sub(created)
	r := rng.Float64()
	step := func(d time.Duration) time.Time {
		t := created.Add(d)
		if t.After(anchor) {
			return anchor
		}
		return t
	}
	switch {
	case age < 24*time.Hour:
		switch {
		case r < 0.85:
			return "placed", created
		case r < 0.95:
			return "shipped", step(6 * time.Hour)
		}
		return "cancelled", step(2 * time.Hour)
	case age < 4*24*time.Hour:
		switch {
		case r < 0.25:
			return "placed", created
		case r < 0.85:
			return "shipped", step(20 * time.Hour)
		case r < 0.93:
			return "delivered", step(70 * time.Hour)
		}
		return "cancelled", step(5 * time.Hour)
	case age < 10*24*time.Hour:
		switch {
		case r < 0.22:
			return "shipped", step(24 * time.Hour)
		case r < 0.89:
			return "delivered", step(96 * time.Hour)
		case r < 0.97:
			return "cancelled", step(6 * time.Hour)
		}
		return "returned", step(8 * 24 * time.Hour)
	}
	switch {
	case r < 0.89:
		return "delivered", step(4*24*time.Hour + time.Duration(rng.IntN(72))*time.Hour)
	case r < 0.97:
		return "cancelled", step(time.Duration(1+rng.IntN(30)) * time.Hour)
	}
	return "returned", step(time.Duration(9+rng.IntN(10)) * 24 * time.Hour)
}

// loadAlerts generates SOC alerts for the same tenants (W9): one alert per
// ten orders, Mitigata's shape of data on the same engine.
func loadAlerts(ctx context.Context, pool *pgxpool.Pool, p *storePlan, rng *rand.Rand, cv *curve, anchor time.Time) (int64, error) {
	n := p.orders / 10
	assets := int64(200 + p.orders/2000)
	times := make([]int64, n)
	for i := range times {
		times[i] = cv.sample(rng).UnixMicro()
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	i := 0
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"soc_alerts"},
		[]string{"tenant_id", "alert_id", "severity", "source_product", "tactic", "status", "asset_id", "risk_score", "analyst_note", "created_at", "updated_at"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if i >= len(times) {
				return nil, nil
			}
			created := time.UnixMicro(times[i])
			i++
			sev := weighted(rng, socSeverity, func(x struct {
				name   string
				weight float64
			}) float64 {
				return x.weight
			}).name
			risk := map[string]int{"critical": 90, "high": 70, "medium": 45, "low": 20, "info": 5}[sev] + rng.IntN(10)
			status := "resolved"
			age := anchor.Sub(created)
			switch r := rng.Float64(); {
			case age < 2*time.Hour && r < 0.7:
				status = "new"
			case age < 24*time.Hour && r < 0.5:
				status = "triaged"
			case age < 72*time.Hour && r < 0.3:
				status = "investigating"
			case r < 0.18:
				status = "false_positive"
			}
			note := ""
			if rng.IntN(4) == 0 {
				note = "Analyst note (synthetic)"
			}
			return []any{p.id, int64(i), sev, socSources[rng.IntN(len(socSources))], socTactics[rng.IntN(len(socTactics))], status,
				1 + rng.Int64N(assets), int32(risk), note, created, created.Add(time.Duration(rng.IntN(48)) * time.Hour)}, nil
		}))
	return int64(n), err
}
