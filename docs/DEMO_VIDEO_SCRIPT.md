# Demo video script

About 2.5 minutes. Record `/monitor` at 1280x720 on the main tier. Numbers are from our runs; replace them with what your recording shows.

| Time | Shot | Voice-over |
|---|---|---|
| 0:00 | Recorder on Direct | "Ten million orders, a hundred stores, a thousand dashboards querying the main database. The app's own query time climbs." |
| 0:20 | Click Cuboid | "Same load, same database. Dashboard queries on the main database drop to zero." |
| 0:40 | Store 42 page | "The PRD's four questions on the largest store, each saying how fresh it is." |
| 1:00 | Insert, then cancel an order | "A new order shows up in about a second. Cancel it, and it moves." |
| 1:15 | Open a number, press Check against main database | "Any number. Compared at an exact point in the WAL, while writes run. Across 2,500 checks, every one was exact." |
| 1:35 | Isolation page, run a drill | "INC-02 was a cache handing users other users' data. We inject that bug into our own code. Zero leaked." |
| 2:00 | Flood, then YAML | "The biggest store floods and gets limited. Others do not notice. A new breakdown from three lines of YAML." |
| 2:20 | Results page | "This is what we measured, including what we did not hit: one query misses the growth target, and we show it." |
| 2:30 | End | "Fast for every store. Exact to the paisa. Never in the wrong store's hands." |
