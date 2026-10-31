# Threat model (STRIDE)

Assets: tenant data, API keys, main-database availability, aggregate integrity.

A row marked **Designed, not yet tested** has code but no automated test yet. It should not be claimed in the pitch until one exists.

| Category | Threat | Mitigation | Evidence |
|---|---|---|---|
| Spoofing | Guessed or malformed key | 32 random bytes, constant-time HMAC compare, 401 with no detail | Designed, not yet tested as a suite (R2) |
| Spoofing | Another store's key | Scope check, 404 | `bench demo-check` D9 |
| Tampering | SQL in a filter value | Shape validation, then bind parameters; identifiers come only from validated config | Designed, not yet tested (R3) |
| Tampering | Tenant reassignment in source data | Retract from the old store, add to the new, raise a security event | Model test covers the numbers; the event is not yet tested |
| Repudiation | Denying a data access | Audit log of key prefix, query, stores, result size and cache status; 365-day retention is a policy, not yet automated | `internal/audit` |
| Information disclosure | INC-02 cache or connection mix-up | Six checks, drills A and B | `bench demo-check` D17 |
| Information disclosure | IDOR through path ids | Path is only a selector | D9, `bench isolation` |
| Information disclosure | Enumeration through 403 vs 404 or filter errors | Identical 404s; filters checked by shape only | `bench isolation` compares 404 bodies |
| Information disclosure | Timing differences on 404 | Not fully closed: known and unknown ids take different code paths. Documented, not mitigated beyond the shared path | None |
| Information disclosure | Keys or rows in logs | Keys appear only as prefixes; logs hold no rows. Keys in URLs are refused with 401 | Designed, not yet tested |
| Information disclosure | PII in the aggregate store | `pii_columns` are never mirrored; a measure on one is refused | Config validation; schema test not yet written |
| Denial of service | Dashboard storm | The query path never touches the main database | `bench safety` |
| Denial of service | Heavy-query flood | Per-store heavy and light limits kept below the global cap, per-key rate, 429 with `Retry-After` | D13 |
| Denial of service | `n = 10^9`, huge ranges, unknown parameters | Strict validation, 400 or 422 | Designed, not yet tested (R4) |
| Denial of service | Slot fills the main database's disk | `max_slot_wal_keep_size=4GB`, alarm at 1 GB | Designed, not yet tested (R10) |
| Denial of service | Bulk update storm | Coalescing and a size-triggered flush | Scenario control exists; not yet measured |
| Elevation of privilege | Store key to company or operator | Scope is decided only by the key; admin routes require the operator key | D9, D10 for scope; admin route refusal not yet tested |

Compliance relevance is limited: see `docs/JUDGE_FAQ.md`. Cuboid is not a compliance product.
