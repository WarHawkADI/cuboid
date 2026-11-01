# Judge FAQ

Numbers come from `docs/TARGETS.md` and `artifacts/`, measured with every service on one laptop.

**Why not just Redis?**
A cache in front of SQL is fast only on a hit. Every miss and expiry still runs the query on the main database, and on a busy store new orders invalidate it constantly. A shared cache and pooled connection are also the conditions of INC-02. Cuboid has a cache too, but it sits in front of pre-computed rollups, so a miss costs milliseconds on a separate database.

**Why not a nightly job or a materialized view?**
A nightly job shows yesterday's numbers, the PRD's own bad shortcut. A materialized view's refresh runs on the main database. Cuboid applies changes about a second after commit, including updates and deletes.

**Why not ClickHouse or DuckDB?**
They scan faster, and are the right choice for ad-hoc analytics over raw rows. Cuboid needs something else: retractions, mirror rows and the replication position committed in one transaction, so a crash cannot double-count, and row-level security on every read. Postgres on both sides also means one thing to operate. If the query set became ad-hoc, a columnar store would be the better tool.

**How can the numbers match while writes continue, and for arbitrary timestamps?**
A check writes a fence into the WAL and takes a snapshot on the main database. When the stream reaches the fence, Cuboid evaluates the query at exactly that point. Commit order is not visibility order, so transactions on the wrong side of the fence for that snapshot are reconciled using its transaction lists. Then every row is compared. Arbitrary timestamps work because partial buckets are read row by row from the mirror. See ADR-006 and ADR-007.

**Has that check ever caught anything?**
Yes. It found a real bug that lost orders: a commit written straight after a heartbeat shares its LSN, and was treated as already applied. See `docs/POSTGRES_NOTES.md`.

**Is each store's data in its own database?**
Yes, as the PRD's setting describes. The platform database holds the registry of stores and companies; each store's orders, products, customers and alerts sit in `store_<id>`, created from a template and pinned to the store by CHECK constraints. Cuboid streams each database with its own slot and position, loads stores in parallel, and reloads only the affected store after a TRUNCATE, a lost slot or a failed check. Company dashboards and checks span their stores' databases. See ADR-012.

**What does `REPLICA IDENTITY FULL` cost?**
More WAL on updates and deletes, since each carries the old row. Inserts are unaffected. We have not measured the cost yet. The alternative is reading old rows from the mirror (ADR-003).

**What happens if Cuboid is down for an hour?**
Dashboards stop being served, because Cuboid serves them. The app is unaffected. Each store's slot holds WAL meanwhile, with an alarm at 1 GB and a 4 GB cap that protects the main server's disk. On restart every store resumes from its committed position and catches up. If the cap is passed, a store's slot is lost, and Cuboid reloads that store from a snapshot of its database. A one-hour outage has not been run; shorter stops and 20 kills at 10M have.

**What does a restart cost the app?**
While Cuboid catches up, the app is measurably slower on one machine, because both databases share a disk. We saw app p95 rise well above baseline just after a restart, then fall as it settled. Separate machines would remove most of that overlap. We have not tested that.

**Why API keys, when login is not required?**
"Store A can never see Store B's numbers" needs a way to tell who is asking. A key is the minimum. Demo keys derive from a seed so the README works verbatim. Production keys are random.

**Is the baseline a straw man?**
No. The direct path uses the same SQL generator as the verifier, a 40-connection pool, and analytics indexes on `(tenant_id, created_at)`, `(tenant_id, status, created_at)` and `(tenant_id, product_id, created_at)`.

**Which numbers should I distrust?**
- Everything runs on one laptop, so the databases, Cuboid, the app and the load generator compete for the same cores and disk. Absolute numbers will differ on your machine.
- A baseline app p95 under 2 ms leaves little room: 10% is a fraction of a millisecond, so run-to-run noise matters.
- The store app writes far faster than the generated history, so "today" is large after a long run.
- Cuboid has been tested at 100 stores, not 1,000.

**How does it reach 1,000 tenants?**
That is a design, not something built or tested: shard across slots, partition engines by tenant hash, run stateless query nodes invalidated by `NOTIFY`, and partition cells by tenant (ADR-010).

**Can it serve our SOC data?**
The `soc_alerts` table is served by the same engine through `config/metrics.yaml`, with no code specific to it. Its data is synthetic; real SOC schemas have not been tried.

**What is simulated?**
The data, the store app's traffic and the dashboards' traffic. The replication, the rollups, the checks against the main database and the drills' faults are real. The drills inject faults into Cuboid's own code on the local stack.

**Is AI used?**
Not in the product: the PRD lists machine learning as not required. Claude Code wrote most of the project under the team's direction, as `docs/DISCLOSURE.md` states.

**Compliance?**
Cuboid is not a compliance product. Its audit log records every data access. CERT-In's direction asks for 180-day log retention and the DPDP Rules 2025 for one year, so a 365-day retention would cover both, but retention is not yet automated in Cuboid, and this is not legal advice.
