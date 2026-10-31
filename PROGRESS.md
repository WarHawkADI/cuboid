# Cuboid — Progress
Started: 2026-10-31 12:18 IST   Deadline: 2026-11-01 23:59 IST (no demo slot given)   Last update: 2026-10-31 14:33 IST
Machine: 12 logical CPUs / 15.6 GB RAM / about 60 GB free / Windows 11. Docker Desktop is broken on this machine (see Blocked).
Data source: fallback generator (PENDING: organisers' script; provided/ is empty)
Tiers seeded: dev 2 s, small 7 s, main 1m45s. First load of main into Cuboid: 3m25s copy + 6m11s rollups.
Running on: native Postgres 16.4 (tools/devdb.sh) and native services (tools/stack.sh), same settings and ports as docker-compose.yml.

## PRD scorecard
Evidence is the dev tier unless stated. "Not measured" means no artifact exists.
- FR-1 to FR-4: working; demo-check D1-D4 exact against main (dev)
- FR-5: 404 on every route for another store's key; isolation suite 50,000 requests, 0 crossed (dev). The 1M run is not done.
- FR-6: insert, cancel, amount fix, delete, backdated: each visible in about 1 s (dev)
- FR-7: REST API, OpenAPI, Postman, requests.http written. README table not written.
- B-1: in every response; D11 pass
- B-2: from mirror and from main, queries answered throughout; D12 pass. From main stops the stream while it runs, unlike the spec's two-consumer design.
- B-3: D13 pass; others' p99 3.5 to 5.9 ms under flood (dev), which misses the spec's 10% bar
- B-4: YAML to live in under 1 s, exact; D14 pass (dev)
- B-5: page is served; never reviewed in a browser
- T1 speed: small tier uncached p99 4-66 ms. Main: running now.
- T2 freshness: Not measured
- T3 safety: Not measured
- T4 correctness: 400/400 exact under writes (dev). Target run not done.
- T5 growth: Not measured
- Run easily (Docker-only clean clone): NOT TESTED; compose file has never run

## Phase status
- [~] Phase 0 — scaffold done; Docker blocked
- [x] Phase 1 — data, store app
- [~] Phase 2 — engine and model test green at 4,000 sequences; 100,000-step soak not run
- [x] Phase 3 — CDC, backfill, mirror, flush
- [~] Phase 4 — API and isolation work; SSE and SOC alerts not exercised
- [~] Phase 5 — bonuses work; targets measured only partly
- [~] Phase 6 — baselines and tool written; steps never run
- [~] Phase 7 — UI built, never viewed; three directions, screenshots, Playwright and axe not done
- [~] Phase 8 — fenced verify, drills, flood work; UI paths unverified
- [~] Phase 9 — Autopilot written, never run; W9 dataset switch in the UI not built
- [ ] Phase 10 — docs: Postgres notes, isolation, threat model, design started; README, ADRs, FAQ, pitch, disclosure and the rest not written
- [ ] Phase 11 — Win Loop rounds
- [ ] Phase 12 — global audit
- [ ] Phase 13 — championship round

## Current focus
T1 speed at 10M, then T2, T4, T3, T5, scorecard; then the first browser review of the UI.

## Debug log (newest first)
- 14:28 stream at 33 txn/s at 10M, falling behind | hypothesis: `key::text` in the dimension loader defeats the primary key | fix: cast the parameter instead | result: about 400 txn/s, caught up
- 14:12 seeding through the stream would buffer whole stores | fix: seed sets a loading flag and drops slots; Cuboid then takes one snapshot
- 14:05 crash on config reload removing a rollup | hypothesis: pending delta held cells of the removed rollup | fix: switch config atomically with the flush boundary; flush ignores removed rollups | result: D14 green. Same fix closes a gap where a new rollup could miss a transaction.
- 13:58 two orders missing from the mirror, found by fenced verify | hypothesis: commit right after a logical message shares its LSN and looks already applied | fix: messagePosition = lsn - 1 | result: 400/400 exact, mirror identical to main. Test: internal/pipeline/position_test.go
- 13:55 store app inserts failing silently (18,488 errors) | fix: typed parameters; errors now logged. Earlier "under writes" results had no inserts and were re-run.
- 13:55 verify "buffer gap" false alarms on an idle ring | fix: compare with what was actually evicted
- 13:52 others' p99 273 ms under flood | hypothesis: per-store light pool (32) above global cap (16) | fix: light pool 8 | result: 8 ms
- 13:51 crash in drill B | hypothesis: another query's rows have a different shape | fix: assert all rows first, check shape, recover panics in singleflight
- 13:50 cancelled leader failed all coalesced viewers | fix: detach the shared computation from the request context
- 13:49 every retraction failed the count CHECK | hypothesis: CHECK runs before ON CONFLICT | fix: UPDATE plus INSERT WHERE NOT EXISTS
- 13:47 COPY refused under forced RLS | fix: shadow tables get RLS in the swap transaction
- 13:05 model test: cell went negative | generator deleted one order twice in a txn; generator bug

## Blocked / deferred
- Docker Desktop backend crashes: `cannot find registry key "SOFTWARE\Docker Inc.\Docker Desktop"`. Needs a repair or reinstall with admin rights by the team. Until then the README's Docker path is untested.
- Open question: 1,113 failed requests of 1,000 viewers on the small tier. Load generator now records what failed.

## Regression tests still owed (spec 17.5)
Bugs found against real Postgres need database-level tests: CHECK before ON CONFLICT, COPY under RLS, reload removing a rollup, dimension loader using the index, drill B shape. demo-check covers some of these end to end; tests/e2e does not exist yet.

## Deliberate deviations from spec (with reason)
- Dictionary is global per dimension, not per (tenant, dimension): cells are always read under the tenant filter, and filter values are checked by shape so the dictionary cannot be probed.
- Rollup cells live in one table per fact source, so each source's measures are real columns.
- SOC alerts are in config/metrics.yaml rather than a separate file: one engine config serves both sources.
- Rebuild from main stops the stream, swaps shadows, then resumes; not the two-consumer cut-over. Queries keep answering, freshness lags honestly. May be upgraded.
- Cache quota is entries per partition, not bytes.
- Per-key rate 500/s rather than 50/s: a store's viewers share its key, and 50/s would limit normal dashboards.

## Deliberate additions beyond spec (and why they help win)
- Auto-rebuild from main on a failed check, at most once in ten minutes.

## Metrics (latest artifacts)
See artifacts/. Model-based: 4,000 sequences, 0 mismatches. Chaos: 5 kills on dev, all exact, 1.1-1.4 s recovery.

## Win score trajectory
| Round | C1 | C2 | C3 | C4 | C5 | Weighted | Scorecard all Great? | Top remaining gap |
|---|---|---|---|---|---|---|---|---|

## Loop ledger
win rounds: 0 | audit passes: 0 (clean streak: 0) | autopilot rehearsals: 0 (failures: 0)
