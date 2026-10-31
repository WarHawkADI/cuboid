# ADR-005: Tenant-partitioned cache and a response guard, not Redis

## Context

INC-02 was a shared cache returning another user's data after a cancelled request.

## Decision

An in-process LRU per store or company, keyed by canonical query, valid across data versions whose changes miss its time range. A guard checks every response against the key's scope.

## Alternatives

Redis adds a network hop, a shared keyspace and a pooled connection: the conditions of INC-02. It is needed only when several query nodes must share a cache.

## Consequences

Each query node has its own cache; several nodes would invalidate through the existing `NOTIFY`. The quota is in entries, not bytes.

## Evidence

`docs/ISOLATION.md`; drills A and B.
