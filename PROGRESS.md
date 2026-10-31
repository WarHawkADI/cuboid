# Cuboid — Progress
Started: 2026-10-31 12:18 IST   Deadline: 2026-11-01 23:59 IST (no demo slot given)   Last update: 2026-10-31 13:10 IST
Machine: 12 logical CPUs / 15.6 GB RAM / 73 GB free / Windows 11 / Docker Desktop installed (daemon not started yet; deferred at the team's request)
Data source: fallback generator (PENDING: organisers' script — provided/ is empty)
Tiers seeded: none yet

## PRD scorecard
FR-1 … FR-7: engine + planner semantics proven in memory (model-based test); API pending
B-1 … B-5:   pending
T1–T5: pending (needs Postgres)
Run easily (Docker-only clean clone): pending

## Phase status
- [~] Phase 0 — Recon & scaffold (Go 1.23.4 installed to ~/sdk/go; Docker deferred)
- [~] Phase 2 — Engine core: config, contributions/retractions, join dims, coalescing, planner, reference, model test (4,000 sequences green)

## Current focus
Database layer: migrations, seed, CDC, mirror, flush, then the API.

## Debug log (newest first)
- 13:05 model test: cell went negative | hypothesis: generator deleted one order twice in a txn because the reference was updated only at commit | fix: apply each drawn change to the reference immediately | result: green
- 13:08 mutation check: planner retention margin replaced by an unsafe value → model test fails within seconds (test has teeth)

## Blocked / deferred
- Docker daemon start deferred by the team; all DB work is written now and verified once Docker is up.

## Deliberate deviations from spec (with reason)
- Dictionary is global per dimension, not per (tenant, dimension): labels such as "placed" are not tenant data, and cells are always read with the tenant filter, so no label from another store can reach a response. Simpler joins in rebuild SQL.
- Rollup cells live in one table per fact source (rollup_cells_orders, rollup_cells_soc_alerts) so each source's measures are real columns.

## Deliberate additions beyond spec (and why they help win)

## Metrics (latest artifacts)
model-based: 4,000 random sequences × up to 60 transactions, every query type, arbitrary ranges, 0 mismatches

## Win score trajectory
| Round | C1 | C2 | C3 | C4 | C5 | Weighted | Scorecard all Great? | Top remaining gap |
|---|---|---|---|---|---|---|---|---|

## Loop ledger
build-loop iterations: 1 | win rounds: 0 | audit passes: 0 (clean streak: 0) | autopilot rehearsals: 0 (failures: 0)
