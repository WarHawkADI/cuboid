# ADR-010: A single process now

## Context

The target is one laptop; the audience runs 1,000+ client organisations.

## Decision

One Go process with clear modules. One goroutine owns the engine and flusher, so the write path needs no locks.

## Alternatives

Splitting CDC, engine and query services now adds failure modes the PRD does not ask for.

## Consequences

This has not been tested beyond 100 stores. The scale-out path, not built: shard across slots or move to Kafka; partition engines by tenant hash; stateless query nodes invalidated by `NOTIFY`; partition cells and mirror by tenant.

## Evidence

Single-process results come from `bench`; the scale-out path is a design only.
