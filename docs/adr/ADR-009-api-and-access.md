# ADR-009: API shape and access model

## Context

The PRD's example is `GET /tenants/42/orders/count?from=...&to=...`, and login is not required.

## Decision

Keep that shape. Use an API key per store, company or operator. The path id is a selector within the key's scope, and anything else returns the same 404 as a missing id.

## Alternatives

403 confirms a store exists. A tenant header or parameter creates a second place to decide access. Login is explicitly out of scope.

## Consequences

Keys need distributing. Demo keys are derived from a seed so the README works verbatim; production keys are random.

## Evidence

`bench demo-check` D9 and D10; `bench isolation` compares 404 bodies.
