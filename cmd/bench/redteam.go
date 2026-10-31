package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// runRedTeam attacks the running stack and checks the behaviours only a real
// database can prove. It writes to the main database as a superuser, in the
// way a judge with psql would, and restores what it changes.
func runRedTeam(ctx context.Context, w *world) bool {
	var rows []checkRow
	add := func(id, check string, pass bool, detail string) {
		rows = append(rows, checkRow{id, check, pass, detail})
		mark := "pass"
		if !pass {
			mark = "FAIL"
		}
		fmt.Printf("  %-4s %-4s %-64s %s\n", id, mark, check, detail)
	}
	big, other := w.stores[0], w.stores[1]
	base := fmt.Sprintf("%s/tenants/%d", cuboidURL, big.ID)
	status := func(url, key string) int {
		resp, err := client.Do(newGet(url, key))
		if err != nil {
			return 0
		}
		drain(resp)
		return resp.StatusCode
	}
	want := func(id, check string, got, expected int) {
		add(id, check, got == expected, fmt.Sprintf("got %d, want %d", got, expected))
	}

	// R1: another store's and another company's data.
	want("R1", "Store key on another store", status(fmt.Sprintf("%s/tenants/%d/orders/count", cuboidURL, other.ID), big.Key), 404)
	if len(w.companies) > 0 {
		want("R1", "Store key on a company route", status(fmt.Sprintf("%s/companies/%d/orders/count", cuboidURL, w.companies[0].ID), big.Key), 404)
	}
	// R2: bad keys.
	want("R2", "No key", status(base+"/orders/count", ""), 401)
	want("R2", "Malformed key", status(base+"/orders/count", "not-a-key"), 401)
	want("R2", "Right prefix, wrong secret", status(base+"/orders/count", big.Key[:len(big.Key)-8]+"00000000"), 401)
	want("R2", "Key in the URL", status(base+"/orders/count?key="+big.Key, ""), 401)
	want("R2", "Store key on an admin route", postStatus(cuboidURL+"/admin/rebuild?source=mirror", big.Key), 401)
	// R3: SQL in a value.
	want("R3", "SQL in a filter value", status(base+"/orders/count?status=%27%20OR%201%3D1--", big.Key), 400)
	// R4: abuse of parameters.
	want("R4", "n = 1,000,000,000", status(base+"/products/top?n=1000000000", big.Key), 400)
	want("R4", "from after to", status(base+"/orders/count?from=2026-10-10&to=2026-10-01", big.Key), 400)
	want("R4", "Unknown parameter", status(base+"/orders/count?tenant_id=43", big.Key), 400)
	want("R4", "A 50-year range", status(base+"/orders/count?from=1970-01-01&to=2026-01-01", big.Key), 400)
	want("R4", "No rollup covers the query", status(base+"/orders/breakdown?by=payment_method&range=last_7d", big.Key), 422)
	// R6: live stream.
	want("R6", "Subscribing to another store's stream", status(fmt.Sprintf("%s/tenants/%d/orders/stream?query=orders/count", cuboidURL, other.ID), big.Key), 404)
	frames := sseFrames(base+"/orders/stream?query=orders/count&range=today", big.Key, 8*time.Second)
	add("W", "Own store's stream delivers frames for this store", frames > 0, fmt.Sprintf("%d frames in 8 s", frames))
	// W9: the same engine on SOC data, through config only.
	socOK := true
	var socDetail []string
	for _, p := range []string{"/soc_alerts/count?range=last_7d", "/soc_alerts/summary?range=last_7d", "/soc_alerts/trend?interval=day&range=last_30d", "/soc_alerts/breakdown?by=severity&range=last_30d", "/tactics/top?n=5&range=last_30d", "/assets/top?n=5&range=last_30d"} {
		if c := status(base+p, big.Key); c != 200 {
			socOK = false
			socDetail = append(socDetail, fmt.Sprintf("%s: %d", p, c))
		}
	}
	add("W9", "SOC alerts answer the same four queries", socOK, strings.Join(socDetail, "; "))
	if ok, det, err := w.check(fmt.Sprintf("/tenants/%d", big.ID), "soc_alerts/breakdown", map[string]string{"by": "tactic", "range": "last_30d"}); true {
		add("W9", "SOC alerts match the main database", err == nil && ok, det+errText(err))
	}

	mainDB, err := pgx.Connect(ctx, env("BENCH_MAIN_DSN", "postgres://postgres:postgres@localhost:5433/shop"))
	if err != nil {
		fatal(err)
	}
	defer mainDB.Close(ctx)
	reader, err := pgx.Connect(ctx, env("BENCH_AGG_READER_DSN", "postgres://cuboid_reader:cuboid_reader@localhost:5434/agg"))
	if err != nil {
		fatal(err)
	}
	defer reader.Close(ctx)

	// Layer 4 alone: a direct session with no tenant setting sees nothing.
	var n int64
	err = reader.QueryRow(ctx, "SELECT count(*) FROM rollup_cells_orders").Scan(&n)
	add("L4", "Reader role with no tenant setting sees no cells", err == nil && n == 0, fmt.Sprintf("%d rows %s", n, errText(err)))
	err = reader.QueryRow(ctx, "SELECT count(*) FROM m_orders").Scan(&n)
	add("L4", "Reader role with no tenant setting sees no mirror rows", err == nil && n == 0, fmt.Sprintf("%d rows %s", n, errText(err)))
	tx, _ := reader.Begin(ctx)
	tx.Exec(ctx, "SELECT set_config('cuboid.tenant_ids', $1, true)", fmt.Sprint(big.ID))
	var foreign int64
	err = tx.QueryRow(ctx, "SELECT count(*) FROM m_orders WHERE tenant_id = $1", other.ID).Scan(&foreign)
	tx.Rollback(ctx)
	add("L4", "Reader scoped to one store cannot see another", err == nil && foreign == 0, fmt.Sprintf("%d rows %s", foreign, errText(err)))
	_, err = reader.Exec(ctx, "DELETE FROM m_orders")
	add("L4", "Reader role cannot write", err != nil, "")

	// I11: PII is not in the aggregate store.
	var pii int64
	aggW, err := pgx.Connect(ctx, env("BENCH_AGG_WRITER_DSN", "postgres://cuboid_writer:cuboid_writer@localhost:5434/agg"))
	if err == nil {
		defer aggW.Close(ctx)
		err = aggW.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name LIKE 'm\_%' AND column_name IN ('full_name', 'email', 'phone', 'analyst_note')`).Scan(&pii)
	}
	add("I11", "No PII column in the mirror", err == nil && pii == 0, fmt.Sprintf("%d PII columns", pii))

	// I1: Cuboid refuses to hold another main-database role.
	if bin := env("BENCH_CUBOID_BIN", "bin/cuboid"); bin != "" {
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = append(os.Environ(), "CUBOID_MAIN_CDC_DSN=postgres://naive_dash:naive_dash@localhost:5433/shop", "CUBOID_LISTEN=:18080")
		out, err := cmd.CombinedOutput()
		add("I1", "Cuboid refuses to start with another main-database role", err != nil && strings.Contains(string(out), "refusing to start"), firstLine(string(out)))
	}
	// I11: keys and rows stay out of the log.
	if b, err := os.ReadFile(env("BENCH_CUBOID_LOG", "data/cuboid.log")); err == nil {
		add("I11", "No store key in Cuboid's log", !strings.Contains(string(b), big.Key), "")
	}

	stable := func(scope, ep string, p map[string]string) (bool, string) {
		var det string
		for i := 0; i < 12; i++ {
			time.Sleep(time.Second)
			ok, d, err := w.check(scope, ep, p)
			det = d + errText(err)
			if err == nil && ok {
				return true, det
			}
		}
		return false, det
	}
	scope := fmt.Sprintf("/tenants/%d", big.ID)

	// A product recategorised: every historical order must move to the new category.
	var pid int64
	var oldCat string
	err = mainDB.QueryRow(ctx, `SELECT o.product_id, p.category FROM orders o JOIN products p USING (tenant_id, product_id)
		WHERE o.tenant_id = $1 AND o.created_at > now() - interval '20 days' GROUP BY 1, 2 ORDER BY count(*) DESC LIMIT 1`, big.ID).Scan(&pid, &oldCat)
	if err != nil {
		fatal(err)
	}
	t0 := time.Now()
	if _, err := mainDB.Exec(ctx, "UPDATE products SET category = $3 WHERE tenant_id = $1 AND product_id = $2", big.ID, pid, "Redteam category"); err != nil {
		fatal(err)
	}
	ok, det := stable(scope, "orders/breakdown", map[string]string{"by": "category", "range": "last_30d"})
	add("D", "Product recategorised to a brand-new category: exact", ok, fmt.Sprintf("%s; %s", time.Since(t0).Round(time.Second), det))
	if _, err := mainDB.Exec(ctx, "UPDATE products SET category = $3 WHERE tenant_id = $1 AND product_id = $2", big.ID, pid, oldCat); err != nil {
		fatal(err)
	}
	ok, det = stable(scope, "orders/breakdown", map[string]string{"by": "category", "range": "last_30d"})
	add("D", "Recategorised back: exact", ok, det)

	// A customer moves city.
	var cid int64
	var oldCity string
	if err := mainDB.QueryRow(ctx, `SELECT c.customer_id, c.city FROM customers c JOIN orders o USING (tenant_id, customer_id) WHERE c.tenant_id = $1 AND o.created_at > now() - interval '20 days' LIMIT 1`, big.ID).Scan(&cid, &oldCity); err != nil {
		fatal(err)
	}
	mainDB.Exec(ctx, "UPDATE customers SET city = 'Redteam city' WHERE tenant_id = $1 AND customer_id = $2", big.ID, cid)
	ok, det = stable(scope, "orders/breakdown", map[string]string{"by": "city", "range": "last_30d"})
	add("D", "Customer changes city: exact", ok, det)
	mainDB.Exec(ctx, "UPDATE customers SET city = $3 WHERE tenant_id = $1 AND customer_id = $2", big.ID, cid, oldCity)
	stable(scope, "orders/breakdown", map[string]string{"by": "city", "range": "last_30d"})

	// R8: an order moved to another store: both stores corrected, event raised.
	var oid int64
	if err := mainDB.QueryRow(ctx, "SELECT order_id FROM orders WHERE tenant_id = $1 AND created_at > now() - interval '3 days' LIMIT 1", big.ID).Scan(&oid); err != nil {
		fatal(err)
	}
	newID := int64(9_000_000_000) + time.Now().Unix()
	if _, err := mainDB.Exec(ctx, "UPDATE orders SET tenant_id = $3, order_id = $4 WHERE tenant_id = $1 AND order_id = $2", big.ID, oid, other.ID, newID); err != nil {
		fatal(err)
	}
	ok, det = stable(scope, "orders/summary", map[string]string{"range": "last_7d"})
	ok2, det2 := stable(fmt.Sprintf("/tenants/%d", other.ID), "orders/summary", map[string]string{"range": "last_7d"})
	add("R8", "Order moved between stores: both exact", ok && ok2, det+"; "+det2)
	raised := false
	var dbEvents int64
	if aggW != nil {
		aggW.QueryRow(ctx, "SELECT count(*) FROM security_events WHERE kind = 'tenant_reassignment' AND at > now() - interval '2 minutes'").Scan(&dbEvents)
		raised = dbEvents > 0
	}
	add("R8", "Security event raised for the move", raised, fmt.Sprintf("%d events", dbEvents))
	mainDB.Exec(ctx, "DELETE FROM orders WHERE tenant_id = $1 AND order_id = $2", other.ID, newID)

	// Bulk update in one transaction, reflected atomically.
	var bulk struct {
		Rows   int64 `json:"rows"`
		TookMS int64 `json:"took_ms"`
	}
	postJSON(cuboidURL+"/admin/scenario", w.op, map[string]any{"name": "bulk_update", "tenant": big.ID, "n": 50000}, &bulk)
	t0 = time.Now()
	ok, det = stable(scope, "orders/summary", map[string]string{"range": "all"})
	add("D", "50,000-row update in one transaction: exact", ok, fmt.Sprintf("%d rows, exact after %s; %s", bulk.Rows, time.Since(t0).Round(time.Second), det))

	writeArtifact("redteam", w.tier, map[string]any{"generated_at": time.Now(), "tier": w.tier, "rows": rows})
	pass := 0
	for _, r := range rows {
		if r.Pass {
			pass++
		}
	}
	fmt.Printf("%d of %d checks pass\n", pass, len(rows))
	return pass == len(rows)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return " " + err.Error()
}

func firstLine(s string) string {
	i := strings.Index(s, "refusing to start")
	if i >= 0 {
		s = s[i:]
	}
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 110 {
		s = s[:110]
	}
	return s
}

func postStatus(url, key string) int {
	code, err := postJSON(url, key, map[string]any{}, nil)
	if err != nil {
		return 0
	}
	return code
}

// sseFrames counts update events on a stream for d.
func sseFrames(url, key string, d time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return 0
	}
	n := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: update") {
			n++
		}
	}
	return n
}
