<p align="center"><img src="docs/img/banner.svg" alt="Cuboid: dashboards p99 2.2 ms, new order visible in 1.1 s, 2,500 of 2,500 exact, 0 of 1,000,000 requests crossed stores" width="100%"></p>

# Mitigata CyberForge submission

**Team iiitdards:** Aditya Rai, Bhuvan Raju

**Problem 02: fast multi-tenant data aggregation.** Live dashboards for 100 stores over 10 million orders, without overloading the database that takes those orders, and without one store ever seeing another's numbers.

| | |
|---|---|
| **Run it** | [Quick start](README.md#quick-start) |
| **The demo** | [Recording](docs/media/Cuboid_Demo.mp4) · [runbook with timings and fallbacks](docs/DEMO_RUNBOOK.md) · [video script](docs/DEMO_VIDEO_SCRIPT.md) |
| **The pitch** | [Deck (PowerPoint)](docs/Cuboid_Final_Deck.pptx) · [PDF](docs/Cuboid_Final_Deck.pdf) · [15 hard questions](docs/PITCH_QA.md) · [judge FAQ](docs/JUDGE_FAQ.md) |
| **The proof** | [Scorecard](docs/TARGETS.md) · [benchmarks](docs/BENCHMARKS.md) · [every PRD line traced](docs/REQUIREMENTS.md) · [raw results](artifacts/) |
| **The honest list** | [Final report: what is done, what is not](docs/FINAL_REPORT.md) |

<p align="center"><img src="docs/img/demo.gif" alt="The demo running at 10M orders" width="100%"></p>

## The problem, in the PRD's words
Dashboards need counts, trends, top-N lists and breakdowns per store. Running those on the main database means "one big store's report makes the app slow for all stores". Caching alone trades that for the failure behind INC-02, where a cache bug showed users other users' data.

## Our answer
Each store's data sits in its own database, as the PRD describes. Cuboid reads every store's database through its own change stream and keeps exact, pre-computed rollups in a separate Postgres. Dashboards, for a store or a whole company, read only from there. A failure in one store's database reloads only that store ([ADR-012](docs/adr/ADR-012-database-per-store.md)).

<p align="center"><img src="docs/img/before-after.svg" alt="Before: dashboards and the store app compete on the main database. After: dashboards read an aggregate store fed by the change stream." width="100%"></p>

Three ideas carry the design:

1. **Exact, not approximate.** Updates and deletes retract the old row and add the new one. Rollup changes, mirror changes and the replication position commit in one transaction, so a crash resumes exactly.
2. **Provable.** Any number can be compared with direct SQL on the main database at the same WAL position, while writes run. This check found a real bug that unit tests could not.
3. **Separated six times over.** We inject INC-02's failure modes into our own code and count the leaks.

## What we measured

<p align="center"><img src="docs/img/safety.svg" alt="With 1,000 dashboards, app p95 is 11.2 ms direct and 3.96 ms through Cuboid" width="100%"></p>

Both PRD sizes: 1M orders over 10 stores and 10M orders over 100 stores, each store in its own database, with 1,000 simulated dashboards. Both database servers, Cuboid, the store app and the load generator share one machine (16 logical CPUs, 14.5 GB of RAM). The table is the 10M tier. Query speed is timed once the machine has settled after a load, at both sizes.

| PRD target | Good | Great | Measured | Verdict |
|---|---|---|---|---|
| Query speed | Under 1 s | Under 200 ms | 1,000 dashboards: p99 2.2 ms, 0 errors. PRD examples: under 28 ms uncached on the largest store. Top customers: median 40 ms, slowest of 5 47 ms | **Great** |
| Freshness | Under 5 min | Under 30 s | p99 1.1 s, 0 of 200 timed out | **Great** |
| Database safety | Under 25% | Under 10% | App p95 3.94 to 3.96 ms, +0.6%. Against Cuboid stopped (2.91 ms), +36.2%. Direct: 11.2 ms | **Great** |
| Correctness | Most queries | All queries | 2,500 of 2,500 exact, 500 during writes | **Great** |
| Growth, 1M to 10M | Under 5x | Under 2x | 1.3x to 3.4x on the other PRD examples. Top customers: 7.8x on its slowest of 5 runs; its median grew 8.3x | **Not met** |

The 1M tier met the first four at Great too: every query's p99 under 16 ms uncached, freshness p99 1.1 s, app p95 +0.5% against Cuboid stopped, 2,500 of 2,500 exact.

| Attack or failure | Result |
|---|---|
| 1,000,000 randomized requests, 57,153 cancelled mid-flight | 0 crossed stores |
| Leak drills A, B, C: a shared cache, a connection mix-up, a missing tenant setting | 0 leaked |
| 20 `kill -9`s under writes | Back within 3.1 s, 500 of 500 checks exact |
| 30 red-team checks: bad keys, injection, row security, recategorising, order moves | 30 of 30 pass |
| 23 acceptance checks: the PRD's examples, both rebuilds, flood, live metric, drills | 23 of 23 pass |

These ran at 10M with all stores in one database. With a database per store, at 1M: 8 `kill -9`s left 200 of 200 checks exact, 30 of 30 red-team checks passed (R8 now checks that a store's database refuses another store's row), and 23 of 23 acceptance checks.

**The slowest row.** Top customers had about 130,000 groups on the largest store and grew 13.7x. It now walks each month's largest customer totals and stops once no one else can make the list, reading a few hundred customers, and every list is still checked against the full result ([ADR-011](docs/adr/ADR-011-exact-top-n.md)). It still grows with the store: the largest store has 6.9 times the orders at 10M, and its median went from 4.8 to 39.7 ms, its slowest of 5 from 6.0 to 46.6 ms. That is 7.8x, which misses the growth target. Timed straight after the load, before autovacuum had run, it measured 11.4x (13.3 to 151 ms).

**Ranges that end now.** "Today", "last 7 days" and "this week" end inside the current hour. That hour used to be read row by row, so the same query slowed as the hour went on: 44 minutes in, top products took 125 ms on the 1M tier. The current hour is now its rollup cell less the rows dated after now, usually none, so it costs the same at any minute ([ADR-007](docs/adr/ADR-007-mirror-and-edges.md)).

<p align="center"><img src="docs/img/growth.svg" alt="Growth from 1M to 10M orders on the largest store: 1.3x to 3.4x on the other queries, 7.8x on top customers" width="90%"></p>

## How it is built

<p align="center"><img src="docs/img/architecture.svg" alt="Architecture: write path from the change stream to rollups, read path through six checks" width="100%"></p>

<p align="center"><img src="docs/img/six-checks.svg" alt="Six isolation checks and where each drill fault was stopped: 0 leaked" width="100%"></p>

## Every PRD line
| PRD | Status | See it |
|---|---|---|
| FR-1 Counts and totals | Done | `GET /tenants/{id}/orders/summary` |
| FR-2 Trends | Done | `GET /tenants/{id}/orders/trend?interval=day` |
| FR-3 Top-N | Done | `GET /tenants/{id}/products/top?n=10&by=sales` |
| FR-4 Breakdowns | Done | `GET /tenants/{id}/orders/breakdown?by=status` |
| FR-5 Tenant isolation | Done | Isolation page, [docs/ISOLATION.md](docs/ISOLATION.md) |
| FR-6 New data, including updates | Done | [Try it yourself](README.md#try-fr-6-yourself) |
| FR-7 API and README | Done | [README](README.md#the-endpoints), [OpenAPI](api/openapi.yaml), [Postman](api/cuboid.postman_collection.json) |
| B-1 Freshness in every response | Done | The `freshness` field |
| B-2 Rebuild from scratch | Done | Controls on `/monitor`. 0 failed queries during rebuilds at 10M |
| B-3 Heavy-query limit | Done | "Flood heavy queries". Others' p99 stayed under 3 ms |
| B-4 New metric by config | Done | Add a line to `config/metrics.yaml`: live in about a minute at 10M |
| B-5 Monitoring page | Done | `/monitor` |

## Beyond the brief
- **Run now.** The scorecard re-measures speed, freshness and correctness on the spot.
- **Same engine, Mitigata's data.** SOC alerts run through config alone: same queries, isolation and checks.
- **Company view.** An owner sees all their stores together, and each separately.
- **Autopilot.** One button drives the whole demo through the real APIs.

## The demo, in four minutes
| # | Beat | What you see |
|---|---|---|
| 1 | The problem | 1,000 dashboards query the main database, and the app's time climbs |
| 2 | Flip to Cuboid | Same load: main-database dashboard queries drop to zero |
| 3 | The PRD, live | The four examples on the largest store, each saying how fresh it is |
| 4 | New data | An order appears in about a second. Cancelled, it moves |
| 5 | Correct | A check at an exact WAL position, with arbitrary timestamps, during writes |
| 6 | Separate | Another store's key gets a 404. The drills inject INC-02: 0 leaked |
| 7 | The bonuses | A flood gets limited, and one line of YAML add a breakdown |
| 8 | The scorecard | The weak rows, said before anyone asks |
| 9 | Close | The new breakdown, now live |

## Rules and disclosure
- **Third parties and AI.** [docs/DISCLOSURE.md](docs/DISCLOSURE.md) lists every library with its licence. Most of the code was written with Claude Code, under the team's direction, and the team can walk through it.
- **Local targets only.** The drills inject faults into Cuboid's own code, run only in demo mode, and touch nothing outside this stack.
- **Data.** Synthetic, from `cmd/seed`. The organisers' script had not arrived when this was built; [provided/README.md](provided/README.md) says how to switch.
- **Docker:** the Compose stack has run on a Linux machine with Docker Engine on 31 October 2026: image build, the 1M and 10M seeds, Cuboid's rebuilds from them, and the `bench` profile. Docker Desktop on the Windows build machine is reported working again. A start from a clean clone (`make docker-clean-clone-check`) has not been recorded. The 12-CPU measurements from before 31 October used the native path, with the same Postgres version, settings and ports.
