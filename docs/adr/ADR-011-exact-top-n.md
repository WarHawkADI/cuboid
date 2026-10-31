# ADR-011: Exact top-N lists from month totals

## Context

A top-N list over a key with many values, such as top customers, had to read one cell per customer and day in the range, then sort them all. Its cost grew with the number of customers, which grows with the store: 13.7x slower from 1M to 10M orders, the one target Cuboid missed.

## Decision

A rank rollup keeps each customer's total per month (`customer_month`, `rank: true`), with an index ordering each month's cells by value. A list over at most three months is then answered exactly by reading a few hundred keys:

1. **Split the range.** A month the range covers in part is either read row by row for the part inside the range, or taken whole from its cell minus the rows outside the range, whichever holds fewer rows up to now. `last_30d` is October's cell minus a day of rows, plus November's cell minus the rows after now, of which there are none. A customer's total is then exactly the sum of their month cells plus their slice rows, with a sign.
2. **Walk the months in descending order** (Fagin's threshold algorithm). Sum every customer seen exactly, by primary key, and stop once the n-th largest total exceeds the sum of the last cell read in each month. Customers whose slice rows add to their total form one more list. A customer not yet seen can neither enter the list nor tie with it.
3. **Rank as the full query does**: value, then store, then key.

The planner never answers a query from a rank rollup. Lists with a filter, longer ranges, or a walk that would read more than 16,384 entries rank every key, as before.

## Alternatives

Running totals for preset windows change the exactly-once flush path, the riskiest code in Cuboid. Approximate top-K sketches cannot match a direct query. A bound from month totals alone (a key below `tau/k` in every month cannot reach `tau`) was built first: it read 186 of 33,240 customers by sales, but about 1,000 by order count, where values are small integers and the 10th-best barely clears the crowd.

## Consequences

- The measure must never be negative. `nonnegative: true` on a sum measure is checked at startup against the main database's `CHECK` constraints; counts qualify by definition.
- The rank rollup adds about one cell per customer and month, and each order updates one more cell. Three partial indexes cover its month cells, one per measure.
- The statements run with plans for their own values. A cached generic plan cannot size an array of keys or months and scanned every month once per key, taking 2.4 s.
- Every answer says how it was found, in `meta.plan.top_bound`.

## Evidence

`internal/query/rank_test.go` compares the walk with ranking every key on random data, with a random split of each month, ties and several stores. `TestSplitRangeTilesTheRange` checks that the split covers each range exactly once. The fenced check compares each pushed-down list with the full result of the same snapshot, and that result with the main database: 150 of 150 exact at 1M, over every store, company scopes, all three measures, n from 1 to 100, every preset and random timestamps.

Measured on 31 October 2026 with `bench speed`, top 10 customers by orders over `last_30d`, at both tiers on one 16-CPU Linux machine:

| | 1M | 10M | Ratio | Before |
|---|---:|---:|---:|---:|
| Largest store, slowest of 5 | 16.2 ms | 59.3 ms | 3.66x | 13.66x |
| All stores, p99 | 16.2 ms | 48.5 ms | 2.99x | 12.64x |

These are the rows in `artifacts/speed/` and `artifacts/growth/latest.json`; the other rows there are from the earlier build. A second 10M run, started straight after a rebuild, gave the same median growth (13.1 to 29.5 ms, 2.3x) but one first-touch sample of 154 ms; top products, unchanged, showed the same (13 ms median, 222 ms slowest). On the same 10M data, the full query this replaces took about 115 ms in SQL alone, before its total for shares.
