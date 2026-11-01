# ADR-010: A single process now

## Context

The target is one laptop; the audience runs 1,000+ client organisations.

## Decision

One Go process with clear modules. One goroutine owns the engine and flusher, so the write path needs no locks.

## Alternatives

Splitting CDC, engine and query services now adds failure modes the PRD does not ask for.

## Consequences

This has not been tested beyond 100 stores. Since ADR-012 every store's database is its own source, with its own slot, stream, position and loads: the unit the scale-out path moves. Not built: split the sources across processes by tenant hash, each with its own engine; stateless query nodes invalidated by `NOTIFY`; partition cells and mirror by tenant; or Kafka.

## Evidence

Single-process results come from `bench`; the scale-out path is a design only.
