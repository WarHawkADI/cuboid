# ADR-003: REPLICA IDENTITY FULL

## Context

To retract an old contribution the engine needs the old row's values.

## Decision

Set `REPLICA IDENTITY FULL` on published tables, so every UPDATE and DELETE carries the complete old row and retraction stays stateless.

## Alternatives

With `DEFAULT`, before-images could be looked up from Cuboid's own mirror. That couples the engine to the mirror's state and is the fallback if the WAL cost ever matters.

## Consequences

More WAL on updates and deletes on the main database. Inserts are unaffected.

## Evidence

The WAL cost has not been measured yet, so no figure is claimed here.
