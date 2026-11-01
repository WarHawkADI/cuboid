# Pitch Q&A

The hardest questions we expect, each with a short answer and where to show it.

1. **Did you meet every target?** Yes, at Good or better. Freshness, correctness and query speed are Great. Database safety and growth are Good. *Show: Results page.*

2. **Why does top customers grow the most?** It used to read every customer in the range, about 130,000 on the largest store, and grew 13.7x. It now walks each month's largest customer totals and stops once no one else can make the list, so it reads a few hundred customers: 3.7x, and still exact. *Show: ADR-011, and `top_bound` in the plan of a top-customers answer.*

3. **Does the dashboard load slow the app?** Direct: app p95 goes to 104 ms. Through Cuboid: 0.99 to 1.16 ms, +17.5%, and +28.4% against Cuboid stopped, because everything shares one laptop's cores and disk. No dashboard query reaches the main database. *Show: recorder.*

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
