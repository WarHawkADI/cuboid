# ADR-012: Each store in its own database

## Context

The PRD's setting: "Each tenants' orders, products and customers sit in their respective database. Every company owner who is having multiple stores wants a dashboard". Cuboid first read one main database in which every store's rows shared tables and were told apart by `tenant_id`.

## Decision

The main server holds one database per store, and one database for the platform:

- **`shop`, the platform's database**, holds the registry of stores and companies and the bulk-load flag.
- **`store_<id>`, each store's own database**, holds that store's orders, products, customers and alerts. It is created from `store_template`, and every table is pinned to its store with `CHECK (tenant_id = <id>)`.

Cuboid streams every database on its own, and keeps everything after the streams as one system:

1. **A stream per database.** Each database has its own replication slot, its own stream, and its own applied position in `cdc_sources`. All the streams feed the single pipeline (ADR-010). Every flush commits the positions of the sources it covers in the same transaction as their data, so exactly-once holds per store. Each row is attributed to the store whose database it was read from.
2. **Loads per store.** A store is loaded from a snapshot of its own database in one aggregate-store transaction: its mirror rows and cells are replaced, and its new slot and consistent point are recorded. Readers see the store's old numbers or its new ones, never a mix. Up to four stores load at once.
3. **Failures stay with their store.** A `TRUNCATE`, a lost slot, a failed check or an operator's request reloads only the affected store. Every other store keeps streaming.
4. **Stores come and go at runtime.** The registry is read every 2 seconds. A new store's database is loaded and streamed without a restart. A store that leaves the registry is stopped, its slots are dropped, and its rows leave the aggregate store in one transaction.
5. **Checks fence each store's database.** A check fences each store's own database and runs the direct query in a snapshot of it. Each stream is held at its fence, so a company's answer is checked whole, against the sum of its stores' direct queries.

The aggregate store, the API, the six isolation checks and company dashboards are unchanged: they already keyed everything by store. Without `tenancy.databases` in the config, Cuboid streams one shared database as before.

## Alternatives

**One pipeline per store** would also isolate each store's flushes. But reloads, config changes and rebuilds from the mirror would then have to coordinate a hundred pipelines at one boundary. **One shared database** with a store column is simpler to run, but the store's data is only separated by a filter, and one store's `TRUNCATE` or lost slot means reloading everyone. **Kafka** between the databases and Cuboid is still the scale-out path (ADR-001), not needed at 100 stores.

## Consequences

- **Connections and limits.** Each store costs two connections (its stream and its heartbeats) and, while it reloads, a second slot. The main server's limits rise to 250 slots and WAL senders, and 800 connections. Each stream decodes with 16 MB instead of 256 MB.
- **WAL is read once per store.** Each slot's WAL sender reads the whole server's WAL and skips other databases' records, so decoding work grows with the number of stores. At 10M, the main server ran 101 WAL senders at about 70% CPU with the store app writing, and every store's dashboard queries ran slower on the shared machine than with one database. With Cuboid running and no dashboards, the store app's p95 rose 35% (2.91 to 3.94 ms); at 1M, with 11 streams, 1.6%. A bulk write in one store's database delays every other store's stream while their senders read past it: 409,000 rows rewritten in `store_43` raised another store's lag to 8.7 s.
- **One stuck stream holds WAL for all.** Every slot shares the server's WAL, so one lagging store holds it for every store. The slot alarm names that slot.
- **Company answers span several positions.** A company's answer combines its stores at their own stream positions, and its freshness is that of its stalest store.
- **Loads and mirror rebuilds wait for each other.** A config reload's rebuild from the mirror and a store's load cannot overlap: each waits for the other.
- **The template must be idle to be copied.** `CREATE DATABASE ... TEMPLATE` refuses a template with a connection open, so Cuboid holds one only while it reads the schema, and the seed retries.

## Evidence

Measured on 31 October 2026, on the 1M tier (10 stores, 10 store databases), with every service on one 16-CPU machine:

| What | Result |
|---|---|
| First load of 11 databases, 4 at a time | 1,270,776 rows in 43 s |
| `TRUNCATE` in `store_43` | Only store 43 reloaded (5 to 13.5 s). No other source reloaded or stopped; their lag stayed at about 1.8 s |
| Slot of `store_44` dropped | Only store 44 reloaded (10 to 11 s). No other source reloaded or stopped |
| Store 47 created and registered at runtime | Streaming 2.7 s later, exact against its database; then deregistered, purged and its database dropped |
| Restart | 12 streams resumed from their committed positions, 0 reloads |
| 8 `kill -9`s under writes | 200 of 200 fenced checks exact |
| Fenced checks on stores and companies | 90 of 90 exact. Company 7 spans 3 databases, held at 3 fences |
| Red team, acceptance checks | 30 of 30 (R8: a store's database refuses another store's row), 23 of 23 |

On the 10M tier (100 store databases), same machine:

| What | Result |
|---|---|
| Seed: 100 store databases created and loaded | 10M orders in 1 m 48 s |
| First load of 101 databases, 4 at a time | 12,713,424 rows in 10 m 44 s; store 42 (1.93M orders) alone took 6 m 35 s. The single-database rebuild took 11 m 30 s |
| Streaming | All 101 sources at about 1 s lag; 101 WAL senders, 388 connections on the main server |
| `TRUNCATE` in `store_43`, slot of `store_44` dropped | Only those stores reloaded (41 s and 6 s); no other source reloaded or stopped |
| Correctness | 150 of 150 fenced checks exact, 50 while writes ran; 30 of 30 on stores and companies |
| Freshness | p99 1.03 s for inserts, 1.07 s for updates |
| Speed, first runs | Every query's p99 across stores under 74 ms; 1,000 dashboards p99 1.5 ms, 0 errors. Store 42's slowest of 5 for top customers: 308 ms, then 191 ms |
| Speed, with the current hour from its cell (ADR-007), once settled after the load | PRD examples under 28 ms on store 42, top customers' slowest of 5 47 ms; every query's p99 across stores under 54 ms; 1,000 dashboards p99 2.2 ms, 0 errors |
| Scorecard run (`bench targets`, 1 November) | 2,500 of 2,500 checks exact, 500 during writes; freshness p99 1.1 s, 0 of 200 timed out; app p95 2.91 ms alone, 3.94 ms with Cuboid running, 3.96 ms with 1,000 dashboards through Cuboid, 11.2 ms with them on the main database |
| Growth from the 1M tier (10 store databases) | Largest store, both tiers settled: 1.3x to 3.4x for the other PRD examples. Top customers 7.8x on its slowest of 5 (6.0 to 46.6 ms), 8.3x on its median (4.8 to 39.7 ms): T5 not met |
