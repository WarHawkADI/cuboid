# Pitch Q&A

The hardest questions we expect, each with a short answer and where to show it.

1. **Did you meet every target?** Four of five, all at Great: query speed, freshness, database safety and correctness. Growth is not met. On the largest store, top customers' slowest of 5 runs grew 7.8x from 1M to 10M and its median 8.3x; every other PRD query grew 1.3x to 3.4x. The scorecard scores the slowest, and so do we. *Show: Results page.*

2. **Why does top customers grow the most?** It used to read every customer in the range, about 130,000 on the largest store, and grew 13.7x. It now walks each month's largest customer totals and stops once no one else can make the list, so it reads a few hundred customers and stays exact. It still grows with the store: the largest store has 6.9 times the orders at 10M, and the answer went from 4.8 to 39.7 ms at the median, 6.0 to 46.6 ms at the slowest of 5. Timed straight after a load, before autovacuum had run, the slowest was 151 ms: slow answers came only while the machine's load average was about 32. *Show: ADR-011, and `top_bound` in the plan of a top-customers answer.*

3. **Does the dashboard load slow the app?** Direct: app p95 goes to 11.2 ms, and dashboards wait 5.0 s at p99. Through Cuboid: 3.94 to 3.96 ms, +0.6%. Cuboid itself is not free: against Cuboid stopped (2.91 ms) it is +36.2% at 10M, because each of 101 store streams reads the whole server's WAL and everything shares one machine's cores and disk. At 1M, with 11 streams, it is +0.5%. No dashboard query reaches the main database. *Show: recorder.*

4. **How do I know the numbers are right?** Press "Check against main database" on any number. It compares at an exact WAL position while writes run. 2,500 of 2,500 were exact at 10M. *Show: number drawer.*

5. **Has that check ever found anything?** Yes, a bug that lost 2 orders in three minutes: a commit straight after a heartbeat shares its LSN, and looked already applied. Unit tests could not see it. *Show: `docs/POSTGRES_NOTES.md`.*

6. **What stops INC-02 here?** Six independent checks. The last re-checks every response against the key's scope, the same redundant check OpenAI added afterwards. We inject that bug into our own code and count leaks. *Show: Isolation page.*

7. **Can a store learn another store exists?** Another store's id and a missing id return the same 404 body, and filter values are checked by shape so the dictionary cannot be probed. Timing differences are not fully closed. *Show: `docs/THREAT_MODEL.md`.*

8. **Why not ClickHouse?** It scans faster and we would use it for ad-hoc analytics. We need retractions, mirror rows and the replication position committed in one transaction, plus row-level security. *Show: ADR-004.*

9. **What if Cuboid crashes mid-write?** The position commits with the data, so it resumes exactly. Twenty `kill -9`s under writes at 10M came back within 3.1 seconds, with 500 of 500 checks exact. The main database also crashed once at 10M, and every store matched afterwards. *Show: Kill Cuboid control.*

10. **What does replication cost the main database?** Extra WAL for `REPLICA IDENTITY FULL` on updates and deletes, which we have not yet measured, plus retained WAL if Cuboid falls behind, capped at 4 GB. *Show: ADR-003.*

11. **What happens during a rebuild?** Queries keep answering from the old numbers, labelled with their true age. Each store reloads from its own database in its own transaction, four at a time, while every other store keeps streaming: one store's TRUNCATE or lost slot reloads only that store. *Show: rebuild control, ADR-012.*

12. **Would it survive 1,000 clients?** Not proven. We tested 100. The path is a design: shard slots, partition by tenant hash, stateless query nodes. *Show: ADR-010.*

12a. **Does each store have its own database, as the PRD says?** Yes. Each store's orders, products and customers sit in their own database, pinned to that store by CHECK constraints, so another store's row cannot be written there. Cuboid streams each database on its own; a new store streams within seconds of being registered. Company dashboards combine their stores' databases. *Show: `psql ... store_42`, ADR-012.*

13. **Could it serve our SOC data?** The `soc_alerts` table runs on the same engine through config, with its numbers matching the main database. It is synthetic data. *Show: `config/metrics.yaml`.*

14. **Can I run this myself?** Yes: `docker compose up -d --build`, then the seed. The Compose stack has run end to end on a Linux machine (build, 1M and 10M seeds, rebuilds, benchmarks); a start from a clean clone has not been recorded yet. *Show: README.*

15. **How much of this did AI write?** Most of it: Claude Code, working to our specification and direction. It is stated in `docs/DISCLOSURE.md`. *Be ready to explain any file yourself.*
