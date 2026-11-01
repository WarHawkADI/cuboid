# Demo runbook

About 4 minutes. Open <http://localhost:8080/monitor>, press **Run demo**, and press **Next** or the space bar for each beat. Autopilot makes the real calls; you speak.

## Before you present
- [ ] Laptop on power, notifications off, browser zoom 100%.
- [ ] Close what you can: Steam, Elasticsearch, MySQL, extra tabs. Memory ran out once on this laptop and took the database down. Aim for 8 GB or more of free commit (`data/mem.log`, or Task Manager).
- [ ] Stack up: `tools/devdb.sh up`, `tools/stack.sh up`. `curl localhost:8080/healthz` says `streaming`.
- [ ] Main tier seeded (about 12 minutes), and data lag about 1 second on the page.
- [ ] With **No dashboards** selected, leave the store app running for 2 minutes, so the baseline is real and the caches are warm.
- [ ] Remove `status_payment` from `config/metrics.yaml` if a rehearsal left it there.
- [ ] Rehearse once. The recording `docs/media/Cuboid_Demo.mp4` is your fallback if the live demo fails.

## The beats
| # | Beat | Say | Point at |
|---|---|---|---|
| 1 | The problem | "One big store's report makes the app slow for all stores: the PRD's words. 1,000 dashboards are querying the main database directly." | The recorder climbing, and queries per second on the main database |
| 2 | Flip to Cuboid | "Same load, same database. Dashboard queries on the main database drop to zero." | The band change, and 0 queries/s on main |
| 3 | The PRD, live | "The four examples on the largest store, from 10 million orders, each saying how fresh it is." | The status line |
| 4 | New data | "Add an order and it shows up. Cancel it and it moves from placed to cancelled." | The millisecond figures |
| 5 | Correct | "Checked against the main database at an exact point in the WAL, with arbitrary timestamps, while writes run." | The verdict, and both timings |
| 6 | Separate | "INC-02. One store's key on another store is not found. Then we inject that exact bug into our own code." | "0 leaked" |
| 7 | The bonuses | "The biggest store floods heavy queries and gets limited. Others do not notice. One line of YAML add a new breakdown, built from the mirror without touching the main database." | The other stores' p99 |
| 8 | The scorecard | Scroll to the targets. Say the weak rows before anyone asks. | The verdict column |
| 9 | Close | "Fast for every store. Exact to the paisa. Never in the wrong store's hands." | The new breakdown, now live |

Beat 9 waits for the YAML rollup to finish building, about a minute at 10M, so talk through the scorecard and Results page while it builds. Before the demo, you can also show the **SOC alerts** switch on a store page and **Run now** on the scorecard, which takes about 30 seconds. The store page's chart spikes on today because the store app has been writing all day: say so if asked.

## Do not claim
- That Cuboid costs the app nothing. 1,000 dashboards through Cuboid measured +0.6%, Great, but against Cuboid stopped the app is +36.2% at 10M.
- That Growth is met. Top customers' slowest run on the largest store grew 7.8x: Not met. Its median grew 8.3x.
- That a clean clone starts with Docker, until `make docker-clean-clone-check` has passed. The stack itself has run under Compose.
- 1,000-tenant scale. It has been tested at 100.

## If something goes wrong
| Problem | Do this |
|---|---|
| A beat fails | Read its message aloud and press Next. Each beat stands alone. |
| The app is slow right after Direct | Direct evicts caches, and the recovery takes a while. Keep talking; do not wait for the line. |
| The page says Cuboid is not answering | The supervisor restarts it within seconds; the page keeps trying. |
| The laptop is struggling | Run on the small tier, or play `docs/media/Cuboid_Demo.mp4`. |
| A check says "did not complete" | A rebuild is running. Try again after it finishes. |
| The database will not start | Windows error 1455 in its log means out of memory. Close applications, then `tools/devdb.sh up`. |
