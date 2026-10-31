# ADR-007: A narrow mirror and exact edges

## Context

Rollups alone cannot answer a range that starts at 11:22:33.

## Decision

Keep the non-PII columns of source rows in AGG, updated in the same transaction as the cells. A range is split into whole months, days and hours, plus edges read row by row from the mirror.

## Alternatives

Rounding ranges to buckets would not match a direct query. Going to the main database for edges would break the first invariant.

## Consequences

The mirror roughly duplicates the fact table's non-PII columns. It also gives rebuilds and new metrics with no main-database load. A query needing too many raw rows gets a 422.

## Evidence

Model test on arbitrary timestamps; `bench demo-check` D8.
