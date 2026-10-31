<p align="center">
  <img src="docs/img/banner.svg" alt="Cuboid: dashboards p99 3 ms, new order visible in 1.1 s, 2,500 of 2,500 exact, 0 of 1,000,000 requests crossed stores" width="100%">
</p>

<p align="center">
  <a href="#quick-start"><b>Quick start</b></a> ·
  <a href="HACKATHON.md"><b>Hackathon submission</b></a> ·
  <a href="docs/media/Cuboid_Demo.mp4"><b>Watch the demo</b></a> ·
  <a href="#the-targets-scorecard"><b>Scorecard</b></a> ·
  <a href="#how-it-works"><b>How it works</b></a> ·
  <a href="docs/Cuboid_Final_Deck.pptx"><b>Deck</b></a> ·
  <a href="docs/PITCH_QA.md"><b>Hard questions</b></a>
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white">
  <img alt="PostgreSQL" src="https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white">
  <img alt="React" src="https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=black">
  <img alt="Tested at" src="https://img.shields.io/badge/tested%20at-10M%20orders%2C%20100%20stores-2846C4">
</p>

**Cuboid sits between the database and the dashboards.** It reads the main database's change stream, keeps exact pre-computed rollups in a separate Postgres, and answers counts, trends, top-N lists and breakdowns in milliseconds for each store. Every number can be checked against the live database, and the checks that keep stores apart are tested by injecting real faults into its own code.

## See it run

<p align="center">
  <img src="docs/img/demo.gif" alt="The demo running: the recorder, flipping dashboards to Cuboid, a new order, a check against the main database, leak drills, a flood and a new metric" width="100%">
</p>

One button drives this through the real APIs, at 10M orders. The [narrated demo video](docs/media/Cuboid_Demo.mp4) runs 2:27, recorded live on this stack at 10M orders.

## The problem, and the answer

"One big store's report makes the app slow for all stores," says the PRD. Caching alone trades that for INC-02, where a cache bug showed users other users' data. Cuboid moves dashboards off the main database entirely, and treats separation as six independent checks.

<p align="center"><img src="docs/img/before-after.svg" alt="Before: dashboards and the store app compete on the main database. After: dashboards read an aggregate store fed by the change stream." width="100%"></p>

<p align="center"><img src="docs/img/safety.svg" alt="With 1,000 dashboards, app p95 is 104 ms direct and 1.16 ms through Cuboid; dashboard p99 is 5.4 s direct and 5.7 ms through Cuboid; the app does 54 operations a second direct and 184 through Cuboid" width="100%"></p>

## Why it is different

| | |
|---|---|
| **Exact, not approximate** | Updates and deletes retract the old row and add the new one. Rollups, mirror and replication position commit in one transaction, so a crash resumes exactly. 2,500 of 2,500 checks matched |
| **Every number is provable** | Press "Check against main database" on any figure. It compares at an exact WAL position, while writes run. This check found a real bug that unit tests could not |
| **Tested by breaking it** | Three drills inject INC-02's failure modes into Cuboid's own code: 0 leaked. 1,000,000 randomized requests with cancellations: 0 crossed stores. 20 `kill -9`s under writes: 500 of 500 checks exact |
| **New metrics are config** | One line of YAML build a new breakdown from the mirror, live in about a minute at 10M, with no restart and no load on the main database |
| **Honest about its limits** | Every target is met, and [the scorecard](#the-targets-scorecard) says which are only Good. [The final report](docs/FINAL_REPORT.md) lists what was left out |

Built for Mitigata CyberForge, Problem 02, by team iiitdards: Aditya Rai and Bhuvan Raju. The submission summary is in [HACKATHON.md](HACKATHON.md).

## Quick start

With Docker:

```
docker compose up -d --build
docker compose --profile seed run --rm seed
```

Then open <http://localhost:8080/monitor>. The seed loads 1M orders across 10 stores, and Cuboid then loads a snapshot of them, which took under a minute on our laptop. For the main tier use `SEED_TIER=main`, which took about 12 minutes in total.

Settings can be changed through `.env`; see `.env.example`. The organisers' data script goes in `provided/`; see `provided/README.md`.

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
| Query speed | Under 1 s | Under 200 ms | With 1,000 dashboards: p99 3 ms, 0 errors. PRD examples under 39 ms uncached on the largest store. Top customers over 30 days: 59 ms uncached | **Great** |
| Freshness | Under 5 min | Under 30 s | p99 1.1 s, inserts and updates | **Great** |
| Database safety | Under 25% slower | Under 10% slower | App p95 0.99 to 1.16 ms with 1,000 dashboards, +17.5%. Against Cuboid stopped (0.91 ms), +28.4%. Direct: 104 ms | **Good** |
| Correctness | Most queries | All queries | 2,500 of 2,500 exact, 500 of them during writes | **Great** |
| Growth | Under 5x | Under 2x | Largest store: 1.2x to 3.2x for PRD examples. Top customers: 3.7x | **Good** |

<p align="center"><img src="docs/img/growth.svg" alt="Growth from 1M to 10M orders on the largest store: 1.2x to 3.2x on most queries, 3.7x on top customers" width="90%"></p>

The two weak spots, plainly:

- **Top customers.** It used to read every customer in the range and grew 13.7x. It now walks each month's largest customer totals and stops once no one else can make the list ([ADR-011](docs/adr/ADR-011-exact-top-n.md)), so it reads a few hundred customers and stays exact. At 3.7x it is still the slowest-growing query: Good, not Great. It was re-measured at both tiers on a 16-CPU machine.
- **Moving ranges under load.** Under heavy dashboard load, a preset such as `last_7d` refreshes about every 5 seconds, so viewers do not queue behind recomputation. Every answer states its true age. Fixed ranges see new orders in about a second.

Also measured at 10M: 1,000,000 randomized requests with 0 crossing stores, and 20 `kill -9`s under writes, back within 3.1 s with 500 of 500 checks exact. Method and raw numbers: `docs/BENCHMARKS.md`. The "Run now" button on `/monitor` re-measures speed, freshness and correctness in about a minute.

## The bonuses

| Bonus | How to see it |
|---|---|
| B-1 Freshness | `freshness` in every response: `"updated": "updated 1 second ago"` |
| B-2 Rebuild from scratch | Controls on `/monitor`: from the mirror with no main-database load, or from main. Queries keep answering; on a rebuild from main the stream pauses and answers show their true age |
| B-3 Heavy-query limit | "Flood heavy queries" on `/monitor`: the flooding store gets 429s, others' p99 stayed flat |
| B-4 Metric by config | Add a rollup to `config/metrics.yaml` while running. It builds from the mirror and is served without a restart |
| B-5 Monitoring page | `/monitor` |

The same engine serves a second dataset through config alone: press **SOC alerts** on a store page.

Security: the Isolation page runs leak drills that inject INC-02's failure modes into Cuboid's own code. See `docs/ISOLATION.md`.

## Screens

| | |
|---|---|
| ![The monitoring page](docs/img/monitor.png) **Monitoring.** The store app's own query time, with the PRD's target lines, the path the dashboards take, and every control. | ![A store's dashboard](docs/img/store.png) **Store.** Each store's numbers, compared with the period before, and how fresh they are. |
| ![Where a number comes from](docs/img/drawer.png) **Any number.** How it was assembled from rollups and mirror edges, and a check against the main database. | ![The isolation page after a drill](docs/img/isolation.png) **Isolation.** Run a leak drill and see which check stopped the fault. |
| ![SOC alerts on the same engine](docs/img/alerts.png) **Same engine, your data.** SOC alerts through config alone. | ![Measured results](docs/img/results.png) **Results.** Every number read from a file in `artifacts/`. |

## How it works

<p align="center"><img src="docs/img/architecture.svg" alt="Architecture: the store app writes to the main database; a stream reader, engine and flush feed an aggregate store; dashboards read through key and path checks, planner, cache and limiter, and a response guard; a budgeted check compares against the main database." width="100%"></p>

### Exactly once
Rollup changes, mirror changes and the replication position commit together. After a crash, the stream resumes from the committed position, and anything already applied is skipped.

```mermaid
sequenceDiagram
  participant M as Main database
  participant C as Cuboid
  participant A as Aggregate store
  M->>C: Committed transactions from the WAL
  C->>C: Retract old rows, add new, into one delta
  C->>A: BEGIN, rollup deltas, mirror rows, applied LSN, COMMIT
  A-->>C: Committed
  C->>M: Confirm the LSN, so its WAL can be freed
  Note over C,A: A crash before COMMIT applies nothing. After it, the applied LSN says where to resume.
```

### Any time range, exact
<p align="center"><img src="docs/img/range-slice.svg" alt="A range is tiled with mirror edges, hours, days and months" width="100%"></p>

### Proving a number
A check writes a fence into the WAL, waits for Cuboid to reach it, and compares both answers at that point, accounting for transactions still in flight. It has caught a real bug: a commit straight after a heartbeat shares its LSN and looked already applied, which lost 2 orders in three minutes.

```mermaid
sequenceDiagram
  participant U as Caller
  participant C as Cuboid
  participant M as Main database
  participant A as Aggregate store
  U->>C: Check this number
  C->>M: Write a fence into the WAL
  M-->>C: The fence arrives through the stream
  C->>A: Read the answer at the fence
  C->>M: Run direct SQL, reconciling in-flight transactions
  C-->>U: Exact match, or the rows that differ
```

### Keeping stores apart
<p align="center"><img src="docs/img/six-checks.svg" alt="Six isolation checks, and where each drill fault was stopped: 0 leaked" width="100%"></p>

### A new metric from YAML

```mermaid
flowchart LR
  Y[One line in config/metrics.yaml] --> V{Valid?}
  V -->|No| E[Keep running, name the bad line]
  V -->|Yes| B[Build a shadow from the mirror]
  B --> S[Switch at a flush boundary]
  S --> L[Live, with no restart]
```

### When things go wrong

| What happens | What Cuboid does | Evidence |
|---|---|---|
| Cuboid is killed under writes | Resumes from its committed position | 20 kills, back within 3.1 s, 500 of 500 exact |
| The biggest store floods heavy queries | That store gets 429s, others stay flat | Over 3,500 limited, others' p99 under 3 ms |
| A number looks wrong | Check it against the main database, then rebuild | 0 failed queries during rebuilds at 10M |
| A bad config is saved | The old config stays live, and the error names the line | Config tests |
| The aggregate store is too busy | The last exact answer is served, labelled with its age | Dashboards' p99 3 ms under load |
| A replication slot falls behind | An alarm at 1 GB, and a 4 GB cap protects the main database's disk | Set and coded; not forced in a test |

### The techniques the PRD names

| Technique | How Cuboid uses it |
|---|---|
| Indexing | Cells keyed and stored by store, rollup, grain and bucket |
| Pre-computing | Exact rollups, retractable under updates and deletes |
| Caching | Per-store partitions with range-aware validity, refreshed in the background |
| Streams | Logical replication with a one-second flush. No polling, no triggers |
| Measuring | `cmd/bench` writes every number to `artifacts/` |

Full design and failure modes: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Decisions and their alternatives: [docs/adr/](docs/adr/).

## Tests and tools

| Command | What it does |
|---|---|
| `make setup` | Downloads Go and npm dependencies |
| `go test ./...` | Unit and fuzz targets, repository-wide invariants, and a model-based test against a brute-force reference |
| `make verify` / `make verify-full` | The quick gate. The full one adds end-to-end tests against a running stack, the 100,000-step soak, `govulncheck` and `npm audit` |
| `make clean-clone-check` / `make docker-clean-clone-check` | Prove a fresh clone builds, or runs the quick start in Docker |
| `make demo-check` | The PRD's examples, verbatim |
| `go run ./cmd/bench redteam` | Bad keys, injection, row security, recategorising, order moves |
| `make isolation` / `make isolation-full` | 100,000 or 1,000,000 randomized requests with cancellations |
| `make chaos` | `kill -9` under writes; needs the supervisor |
| `make targets` | T1 to T5 and the scorecard |
| `make report` | Regenerates `docs/BENCHMARKS.md` from `artifacts/` |
| `make rebuild` | Rebuilds from the main database |
| `make critique` | Screenshots of every view, with blurred and half-size copies for review |
| `make disclosure` | Regenerates the licence tables in `docs/DISCLOSURE.md` |
| `node web/e2e/shots.mjs` | Browser and accessibility check of every screen |
| `node web/e2e/autopilot.mjs` | Runs the demo in a browser and records it |

On Windows without `make`, use `go run ./tools/task <target>`.

## More

| | |
|---|---|
| **Submission** | [HACKATHON.md](HACKATHON.md), [the deck](docs/Cuboid_Final_Deck.pptx), [docs/FINAL_REPORT.md](docs/FINAL_REPORT.md), [docs/DEMO_RUNBOOK.md](docs/DEMO_RUNBOOK.md), [docs/DECK_OUTLINE.md](docs/DECK_OUTLINE.md), [docs/DEMO_VIDEO_SCRIPT.md](docs/DEMO_VIDEO_SCRIPT.md), [docs/PITCH_QA.md](docs/PITCH_QA.md), [docs/JUDGE_FAQ.md](docs/JUDGE_FAQ.md) |
| **Results** | [docs/TARGETS.md](docs/TARGETS.md), [docs/BENCHMARKS.md](docs/BENCHMARKS.md), [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md), [artifacts/](artifacts/) |
| **Design** | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/adr/](docs/adr/), [docs/POSTGRES_NOTES.md](docs/POSTGRES_NOTES.md), [docs/DATA.md](docs/DATA.md), [docs/DESIGN.md](docs/DESIGN.md) |
| **Security** | [docs/ISOLATION.md](docs/ISOLATION.md), [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md), [docs/DISCLOSURE.md](docs/DISCLOSURE.md) |
| **API** | [docs/API.md](docs/API.md), [api/openapi.yaml](api/openapi.yaml), [api/cuboid.postman_collection.json](api/cuboid.postman_collection.json), [api/requests.http](api/requests.http) |
