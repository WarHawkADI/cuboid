# ADR-008: Rebuilds with shadow tables

## Context

The PRD's bonus asks for rebuilding every number if something goes wrong.

## Decision

From the mirror: snapshot AGG at a flush boundary, build shadow cells, replay the logged deltas, swap in one transaction, while the streams continue. From main: since ADR-012, each store reloads from its own database: a new slot and snapshot, its rows replaced and its stream moved to the new slot in one transaction, a few stores at a time.

## Alternatives

Rebuilding in place would serve partial numbers. A two-consumer cut-over for rebuild from main would keep freshness during it, but needs a second engine and flusher running to a common fence.

## Consequences

During a store's reload from its database, that store's answers stay available as of an older point and are labelled by their freshness; every other store keeps streaming. Before ADR-012, a rebuild from main stopped the one stream for about 10 minutes at the main tier. A dimension change during an online rebuild from the mirror is a known gap.

## Evidence

`bench demo-check` D12.
