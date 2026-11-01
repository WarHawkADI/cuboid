# Deck outline

The built deck: [Cuboid_Final_Deck.pptx](Cuboid_Final_Deck.pptx), with speaker notes on every slide, and a [PDF copy](Cuboid_Final_Deck.pdf). Each slide answers a named PRD clause and cites the files behind its numbers.

Ten slides, one idea each. The built deck is the team's private Cuboid pitch deck, with speaker notes. Every number comes from `artifacts/`, measured on one laptop.

0. **Cover.**
1. **The problem.** "One big store's report makes the app slow for all stores." Plus INC-02, in one line: a cache bug showed users other users' data.
2. **Cuboid in one picture.** Change stream in, exact rollups in a separate database, dashboards never touch the main database.
3. **The scorecard.** The PRD's table with measured values, weak rows included. Dashboards p99 2.2 ms; freshness p99 1.1 s; safety +0.6%, and +36.2% against Cuboid stopped; correctness 2,500 of 2,500; growth not met.
4. **The recorder.** Direct against Cuboid on the same load: app p95 11.2 ms against 3.96 ms.
5. **Correct.** How a fenced check works, in three steps. The bug it caught: 2 orders lost, found only because we compare against the live database.
6. **Separate.** Six checks, the drills, 1,000,000 requests and 20 kills: 0 leaked, 0 crossed, 500 of 500 exact.
7. **The bonuses.** One line each. YAML to live metric; flood limited with other stores flat; rebuild while answering.
8. **Growth and limits.** 1M to 10M per query, 1.3x to 3.4x on the other PRD examples. Top customers: its slowest run grew 7.8x and its median 8.3x, which misses the target. Ranges ending now cost the same at any minute of the hour.
9. **Same engine, your data.** SOC alerts through config. Roadmap: 1,000 tenants, not yet built.
