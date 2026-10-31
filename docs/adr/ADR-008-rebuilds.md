# ADR-008: Rebuilds with shadow tables

## Context

The PRD's bonus asks for rebuilding every number if something goes wrong.

## Decision

From the mirror: snapshot AGG at a flush boundary, build shadow cells, replay the logged deltas, swap in one transaction, while the stream continues. From main: a new slot and snapshot, backfill shadows, swap tables and move to the new slot in one transaction.

## Alternatives

Rebuilding in place would serve partial numbers. A two-consumer cut-over for rebuild from main would keep freshness during it, but needs a second engine and flusher running to a common fence.

## Consequences

During a rebuild from main the stream stops, so answers stay available as of an older point and are labelled by their freshness. At the main tier that took about 10 minutes, and the writes made meanwhile are applied afterwards. A dimension change during an online rebuild from the mirror is a known gap.

## Evidence

`bench demo-check` D12.
