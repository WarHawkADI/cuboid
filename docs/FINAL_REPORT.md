# Final report

The project is presentable and works end to end on the native stack at 10M orders. At the team's request, some of the build specification's long verification loops were left out so it could be finished for presentation. This report says which.

## PRD scorecard
| Item | Status | Evidence |
|---|---|---|
| FR-1 to FR-4 | Working | `artifacts/demo-check/dev.json`; 2,500 checks at 10M |
| FR-5 | Working; 50,000-request run, not 1,000,000 | `artifacts/isolation/dev.json`; `artifacts/redteam/main.json` |
| FR-6 | Working: about 1 s | `artifacts/freshness/main.json` |
| FR-7 | API, README, OpenAPI, Postman done | `api/`, `README.md` |
| B-1 | Done | Every response |
| B-2 | Done; a rebuild from main pauses the stream, rather than the two-consumer design | D12 at the dev tier; rebuilt at 10M repeatedly |
| B-3 | Done; others' p99 stayed flat at 10M in the demo | Autopilot run |
| B-4 | Done: about 2.5 minutes to build at 10M, under 1 s at the dev tier | D14; Autopilot run |
| B-5 | Done | `/monitor` |
| T1 Speed | **Good.** 1,000 viewers p99 76 ms; PRD examples under 55 ms on the largest store; top customers 256 ms | `artifacts/speed/main.json` |
| T2 Freshness | **Great.** p99 1.1 s | `artifacts/freshness/main.json` |
| T3 Safety | **Not measured** | Run stopped at the team's request. See risks |
| T4 Correctness | **Great.** 2,500 of 2,500, 500 during writes | `artifacts/correctness/main.json` |
| T5 Growth | **Not met** on top customers (15x); PRD examples 1.2x to 4.5x, most over Great's 2x | `artifacts/growth/latest.json` |
| Runs from the README with Docker | **Not tested** | Docker Desktop is broken on this machine |

## How to run it
See `README.md`. On this machine: `tools/devdb.sh up`, `tools/stack.sh up`, then open <http://localhost:8080/monitor>.

## The demo
`docs/DEMO_RUNBOOK.md`. Autopilot ran all 9 beats with no failures at 10M, once, recorded in `artifacts/demo_walkthrough.webm`. Its YAML beat was changed afterwards to run in the background, and that version has not had a full rehearsal.

## What is simulated
The data, the store app's traffic and the dashboards' traffic. Replication, rollups, checks against the main database and the drills' injected faults are real.

## Known risks, most serious first
1. **The Docker quick start has never run.** Judges may start there. Repair Docker Desktop (it crashes on a missing registry key, `SOFTWARE\Docker Inc.\Docker Desktop`) and run it before presenting. Until then, present from this laptop.
2. **The demo's central claim is limited.** Through Cuboid, dashboard queries on the main database drop to zero, but in the last observation under 1,000 dashboards the app's p95 stayed around 40-60 ms, above the target lines, and an earlier run saw about 4 ms. The cause was not determined; the machine was short of memory and a browser was running. Do not claim the app stays under the 10% line.
3. **Memory.** The laptop ran out once and took down the main database and the store app. Cuboid recovered with no data difference. Close other applications before presenting.
4. **Top customers** misses T1's Great line and T5 on the largest store. The 10M speed figures predate a later speedup, so `make speed-test` should improve them; T5 will likely still miss.
5. **Latest build not fully re-measured.** Correctness and freshness were measured before the final changes to fence handling and the UI.

## Left out of the specification
- T3 safety run, benchmark steps (`make bench`), and pinned-core runs.
- 1,000,000-request isolation run (50,000 done); 20-kill chaos loop (5 done, on the dev tier).
- 100,000-step model soak beyond the 4,000 sequences run. Database-level regression tests in `tests/e2e`; real-database bugs are covered end to end by `demo-check` and `redteam`, not by a dedicated test each.
- Win Loop rounds and the global audit. No panel scores exist, and none are claimed.
- Three explored design directions and fresh-eyes screenshot tests. One direction was built and reviewed by eye.
- W1 "Run now" on the scorecard; W9 dataset switch in the UI (SOC data works through the API).
- Lint, `gosec`, `govulncheck`, `npm audit`.
- The organisers' data script, which has not arrived.

## What to do next
1. Repair Docker and run the README's quick start.
2. Rehearse the demo twice with other applications closed, and watch the recorder under Cuboid.
3. Run `make speed-test` and, with time, `make safety-test` on the main tier, then refresh `docs/TARGETS.md`.
4. Learn the code well enough to answer questions: this is a hiring hackathon, and `docs/PITCH_QA.md` is a starting point.
