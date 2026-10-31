# ADR-006: LSN-fenced verification

## Context

Comparing a live database with a replica normally only works with writes paused.

## Decision

Write a fence into the WAL, take a snapshot on the main database and run the direct SQL. When the stream reaches the fence, evaluate Cuboid at exactly that point. Reconcile transactions whose commit order differs from their visibility using the snapshot's xid lists, then compare every row.

## Alternatives

Pausing writes proves less and is not available in production. Comparing only totals would miss row-level errors.

## Consequences

Each check costs one direct query, so checks are budgeted and limited to the verifier role. Join dimensions in reconciled transactions use current values.

## Evidence

This check found a real lost-transaction bug (`docs/POSTGRES_NOTES.md`).
