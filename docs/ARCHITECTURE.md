# Architecture

Cuboid reads the main database's change stream, keeps exact rollups and a narrow mirror in a separate Postgres, and answers dashboard queries from there. Dashboard queries never reach the main database.

```mermaid
flowchart LR
  App[Store app] -->|normal reads and writes| Main[(Main database)]
  Main -->|logical replication| CDC[cdc: stream and transactions]
  CDC --> Engine[engine: retract old, add new]
  Engine --> Flush[flush: one transaction]
  Flush --> Agg[(Aggregate store: cells, mirror, applied LSN)]
  Dash[Dashboards, API clients] -->|API key| API[query: auth, plan, cache, limit]
  API -->|row-level security| Agg
  API --> Guard[response guard]
  Guard --> Dash
  Verify[verify: fenced check] -.->|budgeted, read-only| Main
  Verify -.-> Agg
```

## Data flow
1. **Stream.** `internal/cdc` reads `pgoutput` and assembles whole transactions. One goroutine owns the engine, the pending delta and the flush cadence, so the write path has no locks.
2. **Contribute.** `internal/engine` turns each change into cell deltas: an insert adds, a delete retracts, an update retracts the old row and adds the new one. It is pure code, so the model test drives exactly what production runs.
3. **Flush.** Every second (or sooner for a large delta or a fence), one AGG transaction writes dictionary values, mirror rows, cell deltas, the applied LSN, data versions and security events.
4. **Query.** `internal/planner` picks the smallest covering rollup and splits the range into whole months, days and hours, plus edge slices from the mirror.
5. **Answer.** Each read runs under row-level security in a REPEATABLE READ transaction. Rows are asserted, then the response passes the guard.

## Why the numbers are exact
- Counts and sums are integers, retractable under any change. `min`, `max` and medians are refused in config.
- Cells and mirror change in one transaction, so they always describe the same point in the stream.
- A range's whole buckets come from cells and its partial buckets from mirror rows, so any timestamp is exact.
- A join dimension (category through products) uses the current value, as a direct join would. When a product is recategorised, that store's affected rollups are recomputed from the mirror inside the same flush.

The model test (`internal/reference`) checks this against a brute-force reference, and the fenced check against the live main database.

## Why each change applies exactly once
The applied LSN commits in the same transaction as the data, and only that committed position is reported to the main database. After a crash the stream resumes from it, and redelivered transactions at or below it are skipped. See the LSN caveat in `docs/POSTGRES_NOTES.md`.

## Keeping busy dashboards fast
- A moving preset such as `last_7d` is cached by name and reused until new data arrives or 30 seconds pass. Its answer echoes the `from` and `to` it was computed for.
- When new data arrives, a request gets the last answer, labelled `stale` with its true `as_of`, while one background refresh runs: for up to 2 seconds on a fixed range, and up to 30 seconds on a moving preset (see Moving ranges under load).
- If the store cannot answer in time, for example at the peak of a rebuild, the newest answer held is served with its true age.
- Top-N lists keyed by product or customer rank in SQL by one measure, and take the total for shares from a smaller rollup. Each verification checks this against the full result of the same snapshot.
- Top customers, whose cost grew with the store (13.7x from 1M to 10M orders), is answered through a rank rollup: each customer's total per month, indexed by value. The range splits into whole-month cells plus thin signed slices of mirror rows. A walk down each month's largest cells, summing every customer it meets, stops once no one unseen can reach the n-th total, so it reads a few hundred customers and stays exact. A filtered list, a range of more than three months, or a walk past its work budget ranks every customer as before. `meta.plan.top_bound` says which path ran. See ADR-011.

## Failure modes
| Failure | What happens |
|---|---|
| Cuboid killed | The supervisor restarts it, and it resumes from the committed LSN. |
| Replication connection lost | It reconnects and resumes. An alarm shows meanwhile. |
| Main database crashes | Cuboid keeps answering with honest freshness, then reconnects. Observed at 10M: no difference after recovery. |
| Aggregate store unavailable | Cached answers are served as stale; otherwise 500 or 503. Not yet tested on purpose. |
| TRUNCATE on the main database | The stream stops and a rebuild from main runs. |
| A check against main fails | Alarm, security event, and a rebuild from main at most once in ten minutes. |
| Slot falls behind | Alarm at 1 GB; `max_slot_wal_keep_size` protects the main database's disk. |
| Slot lost | Detected on reconnect; a rebuild from main runs. |
| A bulk data load | The loader sets a flag and drops the slots; Cuboid waits, then takes one snapshot. |

## Techniques the PRD names
| Technique | How Cuboid uses it |
|---|---|
| Database indexing | Cell primary key is ordered by store, rollup, grain and bucket, and rollups are written in that order so a store's range sits together on disk. Mirror index on store and time for edges. The baseline gets fair analytics indexes. |
| Pre-computing results | Rollup cells per store, grain and dimension values, chosen by the planner. |
| Caching | Per-store partitions, range-aware validity, coalesced requests, refresh in the background. |
| Background jobs or streams | Logical replication, with a one-second flush. No polling, no triggers, no nightly job. |
| Measuring performance | `cmd/bench` writes every published number to `artifacts/`, and the recorder shows the app's own query time live. |

## Scale-out path (a design, not built or tested)
One reader per slot today. At 1,000+ tenants: shard across publications and slots or move to Kafka, partition engines by tenant hash, run stateless query nodes whose caches invalidate through `NOTIFY`, and partition cells and mirror by tenant.

## Moving ranges under load
Every flush invalidates a moving preset such as `last_7d` on any store it touches. Recomputing each one per flush reached hundreds of queries a second with 1,000 dashboards: viewers queued at p50 410 ms, and the extra load slowed the store app on the shared machine. So a preset refreshes at most once a second, every 5 seconds while the aggregate store is at half its concurrency or more, and never makes a viewer wait while its answer is under 30 seconds old. The answer served is exact as of its stated `as_of`. Dashboards' p99 fell to about 3 ms.
