# Final report

Cuboid works end to end at 10M orders and 100 stores, with all PRD features and bonuses built. Each store's data now sits in its own database, as the PRD's setting describes (ADR-012), measured at 1M and 10M. Four of the five PRD targets are met at Great. Growth is not: top customers' slowest answer on the largest store grew 7.8x from 1M to 10M, and its median 8.3x. Two of the build specification's demands also stay open, listed plainly below: a clean-clone start of the Docker path has not been recorded, and the organisers' script has not arrived.

## PRD scorecard
T1 to T5 were measured on 1 November 2026, with each store in its own database, at both PRD sizes (1M orders over 10 stores, 10M over 100) on one machine: every service shares its 16 logical CPUs and 14.5 GB of RAM. Query speed was timed once the machine had settled after a load, at both sizes. The other rows are from 31 October, with all stores in one database, on a 12-CPU laptop (risk 5).

| Item | Status | Evidence |
|---|---|---|
| FR-1 to FR-4 | Working | `artifacts/demo-check/main.json` |
| FR-5 | Working: 0 crossed stores in 1,000,000 requests, 57,153 cancelled mid-flight; identical 404s | `artifacts/isolation/main.json`, `artifacts/redteam/main.json` |
| FR-6 | Working: about 1 s for fixed ranges | `artifacts/freshness/main.json`, D5 to D7 |
| FR-7 | API, README, OpenAPI, Postman done | `api/`, `README.md` |
| B-1 | Done | Every response |
| B-2 | Done: 0 failed queries during rebuilds at 10M. With a database per store, each store reloads in its own transaction while every other store keeps streaming (1M) | D12 on both tiers |
| B-3 | Done: the flooding store limited over 3,500 times, others' p99 under 3 ms | D13 on the main tier |
| B-4 | Done: 45 to 54 s to build at 10M | D14 on the main tier |
| B-5 | Done | `/monitor` |
| T1 Speed | **Great.** 1,000 dashboards p99 2.2 ms, 0 errors. PRD examples under 28 ms on the largest store. Top customers median 40 ms, slowest of 5 47 ms | `artifacts/speed/main.json` |
| T2 Freshness | **Great.** p99 1.1 s, 0 of 200 timed out | `artifacts/freshness/main.json` |
| T3 Safety | **Great.** App p95 3.94 to 3.96 ms with 1,000 dashboards, +0.6%. Against Cuboid stopped (2.91 ms), +36.2%. Direct: 11.2 ms | `artifacts/safety/main.json` |
| T4 Correctness | **Great.** 2,500 of 2,500, 500 during writes | `artifacts/correctness/main.json` |
| T5 Growth | **Not met.** Top customers 7.8x on the largest store's slowest of 5 runs (6.0 to 46.6 ms); its median grew 8.3x. Other queries 1.3x to 3.4x on the largest store | `artifacts/growth/latest.json` |
| The 1M tier | T1 to T4 Great: every query's p99 under 16 ms uncached, freshness p99 1.1 s, app p95 +0.5% against Cuboid stopped, 2,500 of 2,500 exact | `artifacts/*/small.json` |
| Runs from the README with Docker | **Partly shown:** the Compose stack builds, seeds 1M and 10M, rebuilds and benchmarks on Linux. No clean-clone run recorded | `make docker-clean-clone-check` |
| Crash recovery | 20 `kill -9`s under writes: back within 3.1 s, 500 of 500 checks exact | `artifacts/chaos/main.json` |

## How to run it
See `README.md`. On this machine: `tools/devdb.sh up`, `tools/stack.sh up`, then open <http://localhost:8080/monitor>.

## The demo
`docs/DEMO_RUNBOOK.md`. Deck: [`docs/Cuboid_Final_Deck.pptx`](Cuboid_Final_Deck.pptx), with speaker notes, and a [PDF copy](Cuboid_Final_Deck.pdf). Video: [`docs/media/Cuboid_Demo.mp4`](media/Cuboid_Demo.mp4), 2:27, narrated and recorded live at 10M orders; the new metric was built just beforehand, and the video says so. Autopilot ran all 9 beats with no failures at 10M, three times in a row on the final build: 3:30, 2:39 and 2:25.

## What is simulated
The data, the store app's traffic and the dashboards' traffic. Replication, rollups, checks against the main database and the drills' injected faults are real. SOC alerts are synthetic.

## Known risks, most serious first
1. **T5 is not met.** On the largest store, which has 6.9 times the orders at 10M, top customers' slowest of 5 runs grew 7.8x (6.0 to 46.6 ms) and its median 8.3x (4.8 to 39.7 ms). The walk still grows with the store. Timed straight after the load, before autovacuum had run, it measured 11.4x (13.3 to 151 ms): Of 120 samples on the largest store, the slow ones (50 and 112 ms) all came while the machine's load average was 32; at 14 and at 6 the slowest was 31 ms. A list with a filter, or over more than three months, still ranks every customer, as before.
2. **A clean-clone Docker start is not recorded.** The Compose stack has run (image build, 1M and 10M seeds, rebuilds, `bench`) on a Linux machine, and Docker Desktop on the build machine is reported working. Judges may start from the README's Docker path, so run `make docker-clean-clone-check` once and record the result.
3. **The organisers' script has not arrived.** When it does, follow `provided/README.md` and re-measure. If it targets MongoDB, no adapter exists.
4. **Moving ranges lag under load.** Under heavy dashboard load a preset such as `last_7d` refreshes about every 5 seconds, and at most every second otherwise. Answers are served with their true age and stay exact as of that age. Fixed ranges are not throttled.
5. **Two layouts, two machines.** T1 to T5 come from one build, one layout and one machine. The 1,000,000-request run, the 20 kills and B-2 to B-4 at 10M are from 31 October, with all stores in one database, on the 12-CPU laptop. With a database per store, the acceptance checks, the red-team checks and 8 kills were re-run at 1M only (ADR-012).
6. **A database per store costs the main server.** At 10M it runs 101 streams, each WAL sender reading the whole server's WAL: the main database used 70% to 90% of a core with the store app writing, and 12% with it paused. With Cuboid running and no dashboards, the app's p95 rose 35% (2.91 to 3.94 ms); at 1M, with 11 streams, 1.6%. T3 compares with and without dashboards (+0.6%); against Cuboid stopped it is +36.2%. A bulk write in one store briefly delays the others (8.7 s) (ADR-012).
7. **Memory.** The laptop ran out once and took down the main database and the store app. Cuboid recovered with no data difference. The trigger was never found. Close other applications before presenting.
8. **Test-harness races.** The freshness test's probe orders were on the store simulator's list of orders to ship and cancel, so one 10M run lost a probe and timed out. Orders placed through the API are now left alone, and the re-run had 0 of 200 timed out. At 1M, 1,000 dashboards fall on 10 stores, and the per-store limit turned away 1,774 requests with a 429 (B-3); at 10M, none.

## Left out of the specification
- The specification's stricter bars (T1 100 ms, T3 5%, T5 1.5x), and pinned-core runs.
- `make bench` steps: `docs/BENCHMARKS.md` is generated from the targets' artifacts instead.
- 100,000-step model soak: 4,000 sequences were run. `make verify-full` runs the soak.
- Kills timed to land mid-flush, mid-backfill or mid-rebuild; restarting the aggregate store; a 60-second network cut; a 5-minute stop to trip the slot alarm; a measured SSE load; the WAL cost of `REPLICA IDENTITY FULL`; TRUNCATE's automatic rebuild.
- A dimension change during an online rebuild from the mirror can leave that rollup wrong until the next rebuild.
- Win Loop rounds and the global audit. No panel scores exist, and none are claimed.
- Three explored design directions and fresh-eyes screenshot tests.
- Lint, `gosec`. `govulncheck` and `npm audit` are in `make verify-full` but have not been run.
- A database-level regression test for every real-database bug: `tests/e2e_test.go` covers the order lifecycle, and `demo-check` and `redteam` cover the rest end to end.
- MongoDB adapter, per-tenant cache byte quota and the 1,000-tenant build.

## What to do next
1. Repair Docker and run `make docker-clean-clone-check`.
2. Close other applications, then rehearse once while talking: Autopilot has been rehearsed, the talk has not.
3. Learn the code well enough to answer questions: this is a hiring hackathon, and `docs/PITCH_QA.md` is a starting point.
