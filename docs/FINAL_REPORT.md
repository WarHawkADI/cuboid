# Final report

Cuboid works end to end on the native stack at 10M orders and 100 stores, with all PRD features and bonuses built. Three of the build specification's demands stay open, listed plainly below: the Docker path has never run, the organisers' script has not arrived, and one query misses the speed and growth targets.

## PRD scorecard
Measured on one laptop on 31 October 2026. Every service shares its 12 logical CPUs and 15.6 GB of RAM.

| Item | Status | Evidence |
|---|---|---|
| FR-1 to FR-4 | Working | `artifacts/demo-check/main.json` |
| FR-5 | Working: 0 crossed stores in 1,000,000 requests, 57,153 cancelled mid-flight; identical 404s | `artifacts/isolation/main.json`, `artifacts/redteam/main.json` |
| FR-6 | Working: about 1 s for fixed ranges | `artifacts/freshness/main.json`, D5 to D7 |
| FR-7 | API, README, OpenAPI, Postman done | `api/`, `README.md` |
| B-1 | Done | Every response |
| B-2 | Done: 0 failed queries during rebuilds at 10M. A rebuild from main pauses the stream, rather than the two-consumer design | D12 on the main tier |
| B-3 | Done: the flooding store limited over 3,500 times, others' p99 under 3 ms | D13 on the main tier |
| B-4 | Done: 45 to 54 s to build at 10M | D14 on the main tier |
| B-5 | Done | `/monitor` |
| T1 Speed | **Great.** 1,000 dashboards p99 3 ms, 0 errors. PRD examples under 39 ms on the largest store. Top customers 59 ms | `artifacts/speed/main.json` |
| T2 Freshness | **Great.** p99 1.1 s | `artifacts/freshness/main.json` |
| T3 Safety | **Good.** App p95 0.99 to 1.16 ms with 1,000 dashboards, +17.5%. Against Cuboid stopped (0.91 ms), +28.4%. Direct: 104 ms | `artifacts/safety/main.json` |
| T4 Correctness | **Great.** 2,500 of 2,500, 500 during writes | `artifacts/correctness/main.json` |
| T5 Growth | **Good.** Top customers 3.7x, down from 13.7x with month totals (ADR-011). Other queries 1.2x to 3.2x on the largest store | `artifacts/growth/latest.json` |
| Runs from the README with Docker | **Not tested** | Docker Desktop is broken on this machine |
| Crash recovery | 20 `kill -9`s under writes: back within 3.1 s, 500 of 500 checks exact | `artifacts/chaos/main.json` |

## How to run it
See `README.md`. On this machine: `tools/devdb.sh up`, `tools/stack.sh up`, then open <http://localhost:8080/monitor>.

## The demo
`docs/DEMO_RUNBOOK.md`. Deck: [`docs/Cuboid_Final_Deck.pptx`](Cuboid_Final_Deck.pptx), with speaker notes, and a [PDF copy](Cuboid_Final_Deck.pdf). Video: [`docs/media/Cuboid_Demo.mp4`](media/Cuboid_Demo.mp4), 2:27, narrated and recorded live at 10M orders; the new metric was built just beforehand, and the video says so. Autopilot ran all 9 beats with no failures at 10M, three times in a row on the final build: 3:30, 2:39 and 2:25.

## What is simulated
The data, the store app's traffic and the dashboards' traffic. Replication, rollups, checks against the main database and the drills' injected faults are real. SOC alerts are synthetic.

## Known risks, most serious first
1. **The Docker quick start has never run.** Judges may start there. Repair Docker Desktop (it crashes on a missing registry key, `SOFTWARE\Docker Inc.\Docker Desktop`), then run `make docker-clean-clone-check`. Until then, present from this laptop.
2. **The organisers' script has not arrived.** When it does, follow `provided/README.md` and re-measure. If it targets MongoDB, no adapter exists.
3. **Top customers** is Good, not Great, on T5: 3.7x. It now ranks through each customer's month totals and reads a few hundred customers (ADR-011), without touching the exactly-once flush path. A list with a filter, or over more than three months, still ranks every customer, as before.
4. **Moving ranges lag under load.** Under heavy dashboard load a preset such as `last_7d` refreshes about every 5 seconds, and at most every second otherwise. Answers are served with their true age and stay exact as of that age. Fixed ranges are not throttled.
5. **Mixed builds.** The 1M figures behind the growth ratios come from an earlier build. Top customers' speed and growth rows were re-measured on 31 October 2026 with the rank rollup, at both tiers, on a 16-CPU Linux machine; every other row is from the 12-CPU laptop. Correctness, freshness, the 1,000,000-request run and the 20 kills were measured before the final refresh fix, which changed when answers are refreshed, not how they are computed. D1 to D17 ran after that fix, before the reload fix.
6. **Memory.** The laptop ran out once and took down the main database and the store app. Cuboid recovered with no data difference. The trigger was never found. Close other applications before presenting.

## Left out of the specification
- The specification's stricter bars (T1 100 ms, T3 5%, T5 1.5x), and pinned-core runs.
- `make bench` steps: `docs/BENCHMARKS.md` is generated from the targets' artifacts instead.
- 100,000-step model soak: 4,000 sequences were run. `make verify-full` runs the soak.
- Kills timed to land mid-flush, mid-backfill or mid-rebuild; restarting the aggregate store; a 60-second network cut; a 5-minute stop to trip the slot alarm; a measured SSE load; the WAL cost of `REPLICA IDENTITY FULL`; TRUNCATE's automatic rebuild.
- The two-consumer rebuild from main. A dimension change during an online rebuild can leave that rollup wrong until the next rebuild.
- Win Loop rounds and the global audit. No panel scores exist, and none are claimed.
- Three explored design directions and fresh-eyes screenshot tests.
- Lint, `gosec`. `govulncheck` and `npm audit` are in `make verify-full` but have not been run.
- A database-level regression test for every real-database bug: `tests/e2e_test.go` covers the order lifecycle, and `demo-check` and `redteam` cover the rest end to end.
- MongoDB adapter, per-tenant cache byte quota and the 1,000-tenant build.

## What to do next
1. Repair Docker and run `make docker-clean-clone-check`.
2. Close other applications, then rehearse once while talking: Autopilot has been rehearsed, the talk has not.
3. Learn the code well enough to answer questions: this is a hiring hackathon, and `docs/PITCH_QA.md` is a starting point.
