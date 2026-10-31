# Threat model (STRIDE)

Assets: tenant data, API keys, main-database availability, aggregate integrity.

| Category | Threat | Mitigation | Evidence |
|---|---|---|---|
| Spoofing | Guessed or malformed key | 32 random bytes, constant-time HMAC compare, 401 with no detail | `bench redteam` (bad keys), `internal/auth` tests |
| Spoofing | Another store's key | Scope check, 404 | `bench demo-check` D9 |
| Tampering | SQL in a filter value | Shape validation, then bind parameters; identifiers come only from validated config | `bench redteam` (injection); `internal/query` parser tests and fuzz target |
| Tampering | Tenant reassignment in source data | Retract from the old store, add to the new, raise a security event | Model test for the numbers; `bench redteam` for the event |
| Repudiation | Denying a data access | Audit log of key prefix, query, stores, result size and cache status; 365-day retention runs automatically, with a size bound for a laptop | `internal/audit` |
| Information disclosure | INC-02 cache or connection mix-up | Six checks, drills A and B | `bench demo-check` D17 |
| Information disclosure | IDOR through path ids | Path is only a selector | D9, `bench isolation` |
| Information disclosure | Enumeration through 403 vs 404 or filter errors | Identical 404s; filters checked by shape only | `bench isolation` compares 404 bodies |
| Information disclosure | Timing differences on 404 | Not fully closed: known and unknown ids take different code paths. Documented, not mitigated beyond the shared path | None |
| Information disclosure | Keys or rows in logs | Keys appear only as prefixes; logs hold no rows. Keys in URLs are refused with 401 | `tests/invariants_test.go`, `tests/e2e_test.go` |
| Information disclosure | PII in the aggregate store | `pii_columns` are never mirrored; a measure on one is refused | `internal/config` tests; red team reads the mirror's columns |
| Information disclosure | A web page reading the API from another site | Browsers are answered only from the UI's own origin or `CUBOID_CORS_ORIGINS`; CSP, `frame-ancestors 'none'`, `nosniff` | `tests/e2e_test.go` |
| Denial of service | Audit or event tables filling the disk | Events capped per kind per second with a summary row; tables pruned by age and size | `internal/audit` tests |
| Denial of service | Dashboard storm | The query path never touches the main database | `bench safety` |
| Denial of service | Heavy-query flood | Per-store heavy and light limits kept below the global cap, per-key rate, 429 with `Retry-After` | D13 |
| Denial of service | `n = 10^9`, huge ranges, unknown parameters | Strict validation, 400 or 422 | `bench redteam`, `internal/query` parser tests |
| Denial of service | Slot fills the main database's disk | `max_slot_wal_keep_size=4GB`, alarm at 1 GB | Set and alarm coded; not forced in a test |
| Denial of service | Bulk update storm | Coalescing and a size-triggered flush | Scenario control exists; run in the demo, no dedicated measurement |
| Elevation of privilege | Store key to company or operator | Scope is decided only by the key; admin routes require the operator key | D9, D10, `tests/e2e_test.go` |

Compliance relevance is limited: see `docs/JUDGE_FAQ.md`. Cuboid is not a compliance product.
