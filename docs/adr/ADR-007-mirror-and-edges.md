# ADR-007: A narrow mirror and exact edges

## Context

Rollups alone cannot answer a range that starts at 11:22:33.

A range that ends now, such as today or the last 7 days, always ends inside the current hour. Read row by row, that edge holds every order since the hour began, so the same query got slower as the hour went on. On the 1M tier the store app adds about 17,000 orders an hour to the largest store: 44 minutes into the hour, its count for the last 7 days read 12,631 mirror rows and took 15 ms, against 2.3 ms when the hour held almost none of the store app's orders. Top products for this week went from 15.7 ms to 125 ms.

## Decision

Keep the non-PII columns of source rows in AGG, updated in the same transaction as the cells. A range is split into whole months, days and hours, plus edges read row by row from the mirror.

An edge is answered from its whole hour's cell less the rows of that hour outside the range, when those hold less time before now than the edge does. Time after now holds no rows, or a few dated early, so the current hour is its cell less the rows dated after now: usually none. An edge in the past keeps whichever side is shorter. Top-N lists already do the same with month totals (ADR-011).

## Alternatives

Rounding ranges to buckets would not match a direct query. Going to the main database for edges would break the first invariant.

## Consequences

The mirror roughly duplicates the fact table's non-PII columns. It also gives rebuilds and new metrics with no main-database load. A query needing too many raw rows gets a 422.

Subtracting is exact because every measure is a count or a sum, and a flush changes a cell and its mirror rows in one transaction. A query's cost no longer depends on the minute of the hour it is asked. A store that dates many orders ahead of time pays for reading those instead.

## Evidence

Model test on arbitrary timestamps, with the planner's clock up to an hour past the data's so ranges end in an hour that holds rows dated later; with the subtraction's sign flipped, it fails. Planner tests count every instant of a range once and every instant outside it not at all. `bench demo-check` D8. With this change, `bench speed` at 10M timed every PRD example except top customers under 28 ms on the largest store at its slowest of 5. On the 10M tier, with an order dated 15 minutes ahead, 18 of 18 fenced checks were exact for store 42 and company 1: counts, sums, breakdowns, hourly and daily trends and top products over today, the last 7 and 30 days and this week.
