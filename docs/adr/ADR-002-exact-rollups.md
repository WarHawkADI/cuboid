# ADR-002: Exact rollups, not sketches

## Context

The PRD compares numbers with a direct query, and Great means matching on all queries.

## Decision

Keep exact integer counts and sums per bucket and dimension, retractable under updates and deletes. The planner picks the smallest covering rollup, the view-selection idea from Harinarayan, Rajaraman and Ullman, "Implementing Data Cubes Efficiently" (1996).

## Alternatives

HyperLogLog and Count-Min are smaller but approximate, so they cannot match. `min`, `max` and medians are not retractable and are refused in config with a reason.

## Consequences

Storage grows with buckets times dimension values: at 10M orders the main tier holds about 31M cells, dominated by rollups keyed by customer or product. A query no rollup covers gets a 422 instead of a slow path.

## Evidence

`internal/reference/model_test.go` checks every query type against a brute-force reference.
