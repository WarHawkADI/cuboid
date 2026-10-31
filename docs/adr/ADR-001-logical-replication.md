# ADR-001: CDC by logical replication

## Context

Dashboards must stay fresh without loading the main database.

## Decision

Read changes through a `pgoutput` logical replication slot. Backfill from the slot's exported snapshot, so there is no gap and no overlap.

## Alternatives

Polling needs an `updated_at` index, misses deletes and adds read load. Triggers put cost inside every app write. Dual writes can diverge. A nightly job is the PRD's own bad shortcut. Debezium and Kafka work but add infrastructure the PRD does not need.

## Consequences

The slot retains WAL while Cuboid is behind, so `max_slot_wal_keep_size` and an alarm bound it. The exported snapshot holds back vacuum during a backfill. The production alternative is chunked snapshots with watermarks (Netflix's DBLog).

## Evidence

`docs/POSTGRES_NOTES.md`; `bench demo-check` D5 to D7.
