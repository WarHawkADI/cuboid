# ADR-004: A separate aggregate store

## Context

Keeping rollups in the main database would still load it.

## Decision

Run a second Postgres instance for rollups and the mirror, with only writer and reader roles there.

## Alternatives

Summary tables in the main database share its CPU, memory and I/O. ClickHouse or DuckDB are faster at scans, but this design relies on committing retractions, mirror rows and the LSN in one transaction.

## Consequences

One more database to run. Postgres on both sides keeps one operational skill and gives row-level security for free.

## Evidence

`bench safety` measures the app's query time; `cdc.CheckRole` refuses any other main-database role at startup.
