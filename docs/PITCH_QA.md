# Pitch Q&A

The hardest questions we expect, each with a short answer and where to show it.

1. **Did you meet every target?** No. Freshness and correctness are Great. Query speed is Good because of one query, top customers, on the largest store; database safety has no final number; growth misses on that same query. *Show: Results page.*

2. **Why is top customers slow?** Its cost grows with the customers in the range, about 130,000 on the largest store, and rollups cannot shrink that. Viewers get it from cache in milliseconds, and the direct query was slower than ours in our checks. *Show: Results page, drawer on a top-N number.*

3. **Does the dashboard load slow the app?** On the direct path it clearly does. Through Cuboid, no dashboard query reaches the main database, but on one laptop the app still slowed under 1,000 dashboards, because everything shares a disk and cores. A short test with no dashboards showed no cost once settled. *Show: recorder.*

4. **How do I know the numbers are right?** Press "Check against main database" on any number. It compares at an exact WAL position while writes run. 2,500 of 2,500 were exact at 10M. *Show: number drawer.*

5. **Has that check ever found anything?** Yes, a bug that lost 2 orders in three minutes: a commit straight after a heartbeat shares its LSN, and looked already applied. Unit tests could not see it. *Show: `docs/POSTGRES_NOTES.md`.*

6. **What stops INC-02 here?** Six independent checks. The last re-checks every response against the key's scope, the same redundant check OpenAI added afterwards. We inject that bug into our own code and count leaks. *Show: Isolation page.*

7. **Can a store learn another store exists?** Another store's id and a missing id return the same 404 body, and filter values are checked by shape so the dictionary cannot be probed. Timing differences are not fully closed. *Show: `docs/THREAT_MODEL.md`.*

8. **Why not ClickHouse?** It scans faster and we would use it for ad-hoc analytics. We need retractions, mirror rows and the replication position committed in one transaction, plus row-level security. *Show: ADR-004.*

9. **What if Cuboid crashes mid-write?** The position commits with the data, so it resumes exactly. Five `kill -9`s under writes came back in about 1.2 seconds and stayed exact, at the dev tier. The main database also crashed once at 10M, and every store matched afterwards. *Show: Kill Cuboid control.*

10. **What does replication cost the main database?** Extra WAL for `REPLICA IDENTITY FULL` on updates and deletes, which we have not yet measured, plus retained WAL if Cuboid falls behind, capped at 4 GB. *Show: ADR-003.*

11. **What happens during a rebuild?** Queries keep answering from the old tables, labelled with their true age. A rebuild from main pauses the stream and took about 10 minutes at 10M; writes made meanwhile are applied afterwards. *Show: rebuild control.*

12. **Would it survive 1,000 clients?** Not proven. We tested 100. The path is a design: shard slots, partition by tenant hash, stateless query nodes. *Show: ADR-010.*

13. **Could it serve our SOC data?** The `soc_alerts` table runs on the same engine through config, with its numbers matching the main database. It is synthetic data. *Show: `config/metrics.yaml`.*

14. **Can I run this myself?** The native path is what we measured on. The Compose files are written, but Docker was broken on our machine, so that path has not been run. *Show: README.*

15. **How much of this did AI write?** Most of it: Claude Code, working to our specification and direction. It is stated in `docs/DISCLOSURE.md`. *Be ready to explain any file yourself.*
