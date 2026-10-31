# Cuboid

Cuboid sits between the database and the dashboards. It reads the main database's change stream, keeps exact pre-computed rollups in a separate Postgres, and answers counts, trends, top-N lists and breakdowns in milliseconds for each store, with every number checkable against the live database.

Built for Problem 02 of Mitigata CyberForge: multi-tenant aggregation.

![The monitoring page](docs/img/monitor.png)

## Quick start

With Docker:

```
docker compose up -d --build
docker compose --profile seed run --rm seed
```

Then open <http://localhost:8080/monitor>. The seed loads 1M orders across 10 stores, and Cuboid then loads a snapshot of them, which took under a minute on our laptop. For the main tier use `SEED_TIER=main`, which took about 12 minutes in total.

**Status of this path:** Docker Desktop was broken on the machine this was built on, so the Compose files have not been run yet. Everything below was measured on the native path, with the same Postgres version, settings and ports:

```
tools/devdb.sh up          # two Postgres 16 clusters; set PGBIN to its bin directory
go build -o bin/ ./cmd/... && npm --prefix web ci && npm --prefix web run build
tools/stack.sh up          # Cuboid, the store app, the direct baseline, the load generator
bin/seed --tier small
```

## Demo keys

A key belongs to one store, one company, or the operator. These are reproducible development defaults from `CUBOID_DEMO_KEY_SEED`, so the examples below work verbatim. Production keys are random. `make keys` prints them.

| Scope | Key |
|---|---|
| Store 42 | `cub_store_42_577cdf7f676643ab6ce505d9d92f8577ec64b9ac798b2170cb0cc13590a16ffc` |
| Store 43 | `cub_store_43_bacfa615cddb5e9a07e82b32d51be2fdd617495d37fdb1df679d3711c6c47b32` |
| Company 7 (stores 42, 43 and one more) | `cub_co_7_e05b322d9e5f3b590135eb755abe64266e622c418c2560bdd01767f0f03fe68e` |
| Operator | `cub_op_193ac3fe1b7f34ec044b882f7651717671bbc0f0b916a61780caaf4fd67ab58f` |

```
export KEY=cub_store_42_577cdf7f676643ab6ce505d9d92f8577ec64b9ac798b2170cb0cc13590a16ffc
```

## The endpoints

| PRD | Endpoint |
|---|---|
| FR-1 | `GET /tenants/{id}/orders/count`, `GET /tenants/{id}/orders/summary` |
| FR-2 | `GET /tenants/{id}/orders/trend?interval=hour\|day\|month` |
| FR-3 | `GET /tenants/{id}/products/top?n=10&by=sales`, `customers/top`, `cities/top` |
| FR-4 | `GET /tenants/{id}/orders/breakdown?by=status\|category\|channel\|city` |
| FR-5 | The key decides. Another store's id returns 404, the same as one that does not exist |
| B-1 | `freshness` in every response |
| Companies | The same paths under `/companies/{id}`, plus `breakdown?by=store` |
| Check | `POST /tenants/{id}/verify` |

Ranges: `from` (inclusive) and `to` (exclusive), or `range=today|yesterday|this_week|last_7d|this_month|last_30d|last_90d|this_year|all`. Details are in `docs/API.md`, `api/openapi.yaml` and the Postman collection, `api/cuboid.postman_collection.json`.

### The PRD's four examples

Every response echoes the absolute `from` and `to` it used. Presets move with the clock, so use those instants in your own SQL. To compare, connect with `psql -h localhost -p 5433 -U postgres shop`.

Number of orders and total sales in the last 7 days:

```
curl -H "Authorization: Bearer $KEY" "localhost:8080/tenants/42/orders/summary?range=last_7d"
```
```sql
SELECT count(*), sum(amount_paise) / 100.0, sum(quantity) FROM orders
WHERE tenant_id = 42 AND created_at >= '<from>' AND created_at < '<to>';
```

Orders per day for the last 30 days:

```
curl -H "Authorization: Bearer $KEY" "localhost:8080/tenants/42/orders/trend?interval=day&range=last_30d"
```
```sql
SELECT date_trunc('day', created_at, 'Asia/Kolkata'), count(*) FROM orders
WHERE tenant_id = 42 AND created_at >= '<from>' AND created_at < '<to>' GROUP BY 1 ORDER BY 1;
```

Top 10 products by sales this week:

```
curl -H "Authorization: Bearer $KEY" "localhost:8080/tenants/42/products/top?n=10&by=sales&range=this_week"
```
```sql
SELECT product_id, sum(amount_paise) / 100.0 AS sales FROM orders
WHERE tenant_id = 42 AND created_at >= '<from>' AND created_at < '<to>'
GROUP BY 1 ORDER BY sales DESC, product_id LIMIT 10;
```

Orders by status:

```
curl -H "Authorization: Bearer $KEY" "localhost:8080/tenants/42/orders/breakdown?by=status&range=last_7d"
```
```sql
SELECT status, count(*) FROM orders
WHERE tenant_id = 42 AND created_at >= '<from>' AND created_at < '<to>' GROUP BY 1 ORDER BY 2 DESC;
```

Or let Cuboid compare, at an exact WAL position, while writes continue:

```
curl -X POST -H "Authorization: Bearer $KEY" localhost:8080/tenants/42/verify \
  -d '{"endpoint": "orders/count", "params": {"from": "2026-10-01T11:22:33", "to": "2026-10-08T09:08:07"}}'
```

## Try FR-6 yourself

Insert an order in psql, in a minute that has no other orders:

```sql
INSERT INTO orders (tenant_id, order_id, customer_id, product_id, quantity, amount_paise, status, payment_method, channel, created_at, updated_at)
VALUES (42, 900000001, 1, 1, 1, 50000, 'placed', 'upi', 'web', '2001-01-01 12:00:00+05:30', now());
```

About a second later:

```
curl -H "Authorization: Bearer $KEY" "localhost:8080/tenants/42/orders/breakdown?by=status&from=2001-01-01&to=2001-01-02"
```

Then cancel it and repeat the curl. It moves from placed to cancelled:

```sql
UPDATE orders SET status = 'cancelled' WHERE tenant_id = 42 AND order_id = 900000001;
```

## The targets scorecard

Measured at 10M orders and 100 stores, with every service sharing one laptop (12 logical CPUs, 15.6 GB of RAM). Generated from `artifacts/`; see `docs/TARGETS.md`.

| What we measure | Good | Great | Measured | Verdict |
|---|---|---|---|---|
| Query speed | Under 1 s | Under 200 ms | With 1,000 dashboards: p99 76 ms, 0 errors. All PRD examples under 55 ms uncached on the largest store. Top customers over 30 days: 256 ms uncached on the largest store | **Good**; PRD examples are under Great |
| Freshness | Under 5 min | Under 30 s | p99 1.1 s, inserts and updates | **Great** |
| Database safety | Under 25% slower | Under 10% slower | No final number. See below | **Not measured** |
| Correctness | Most queries | All queries | 2,500 of 2,500 exact, 500 of them during writes | **Great** |
| Growth | Under 5x | Under 2x | Largest store: 1.2x to 4.5x for PRD examples. Top customers: 15x | **Not met** on top customers |

The two weak rows, plainly:

- **Database safety.** With no dashboards, a short test showed the app's query time unchanged by Cuboid once settled. Just after a restart it is slower while Cuboid catches up, on a shared disk. Under 1,000 dashboards on this laptop the app's p95 rose, by less than on the direct path. The full test, `make safety-test`, has not been completed.
- **Top customers.** Its cost grows with the number of customers in the range, which rollups cannot shrink. Viewers read it from cache in milliseconds; the uncached query is the limit.

The 10M speed run predates a later speedup to this query. `make speed-test` takes a few minutes and refreshes it.

## The bonuses

| Bonus | How to see it |
|---|---|
| B-1 Freshness | `freshness` in every response: `"updated": "updated 1 second ago"` |
| B-2 Rebuild from scratch | Controls on `/monitor`: from the mirror with no main-database load, or from main. Queries keep answering; on a rebuild from main the stream pauses and answers show their true age |
| B-3 Heavy-query limit | "Flood heavy queries" on `/monitor`: the flooding store gets 429s, others' p99 stayed flat |
| B-4 Metric by config | Add a rollup to `config/metrics.yaml` while running. It builds from the mirror and is served without a restart |
| B-5 Monitoring page | `/monitor` |

Security: the Isolation page runs leak drills that inject INC-02's failure modes into Cuboid's own code. See `docs/ISOLATION.md`.

## Architecture

```
Store app ──reads, writes──► Main database ──logical replication──► cuboid ──► Aggregate store
                                  ▲                                  (engine,    (cells, mirror,
                       budgeted checks only                           flush,      applied LSN)
                                                                      API, guard)     ▲
Dashboards ──API key──────────────────────────────────────────────────────┴───row security┘
```

| Technique the PRD names | How Cuboid uses it |
|---|---|
| Indexing | Cells keyed and stored by store, rollup, grain and bucket |
| Pre-computing | Exact rollups, retractable under updates and deletes |
| Caching | Per-store partitions with range-aware validity, refreshed in the background |
| Streams | Logical replication with a one-second flush. No polling, no triggers |
| Measuring | `cmd/bench` writes every number to `artifacts/` |

Full design: `docs/ARCHITECTURE.md`.

## Tests and tools

| Command | What it does |
|---|---|
| `go test ./...` | Includes a model-based test against a brute-force reference |
| `make demo-check` | The PRD's examples, verbatim |
| `go run ./cmd/bench redteam` | Bad keys, injection, row security, recategorising, order moves |
| `make isolation` | Randomized requests with cancellations |
| `make chaos` | `kill -9` under writes; needs the supervisor |
| `make targets` | T1 to T5 and the scorecard |
| `node web/e2e/shots.mjs` | Browser and accessibility check of every screen |
| `node web/e2e/autopilot.mjs` | Runs the demo in a browser and records it |

On Windows without `make`, use `go run ./tools/task <target>`.

## More

`docs/REQUIREMENTS.md`, `docs/TARGETS.md`, `docs/ARCHITECTURE.md`, `docs/API.md`, `docs/DATA.md`, `docs/POSTGRES_NOTES.md`, `docs/ISOLATION.md`, `docs/THREAT_MODEL.md`, `docs/DESIGN.md`, `docs/DEMO_RUNBOOK.md`, `docs/JUDGE_FAQ.md`, `docs/PITCH_QA.md`, `docs/DISCLOSURE.md`, `docs/DECK_OUTLINE.md`, `docs/DEMO_VIDEO_SCRIPT.md`, `docs/FINAL_REPORT.md`, `docs/adr/`.
