# Deck outline

Nine slides, one idea each. Every number comes from `artifacts/`, measured on one laptop.

1. **The problem.** "One big store's report makes the app slow for all stores." Plus INC-02, in one line: a cache bug showed users other users' data.
2. **Cuboid in one picture.** Change stream in, exact rollups in a separate database, dashboards never touch the main database.
3. **The scorecard.** The PRD's table with measured values, weak rows included. Freshness p99 1.1 s; correctness 2,500 of 2,500.
4. **The recorder.** Screenshot: Direct against Cuboid on the same load. Message: dashboard queries on the main database drop to zero.
5. **Correct.** How a fenced check works, in three steps. The bug it caught: 2 orders lost, found only because we compare against the live database.
6. **Separate.** Six checks, the drill, and 0 leaked.
7. **The bonuses.** One line each. YAML to live metric; flood limited with other stores flat; rebuild while answering.
8. **Growth and limits.** 1M to 10M per query, 1.2x to 4.5x on the PRD's examples, and the honest 15x on top customers.
9. **Same engine, your data.** SOC alerts through config. Roadmap: 1,000 tenants, not yet built.
