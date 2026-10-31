# Tenant isolation

A store only ever sees its own data. This document says how that is enforced and how it is tested.

## The incident this is designed against (INC-02)
On 20 March 2023, ChatGPT used redis-py's asyncio client with Redis Cluster to cache user data. If a request was cancelled after being pushed onto a pooled connection's incoming queue but before its response was popped, the connection was left out of step, and the next unrelated request received the cancelled request's response. About 1.2% of ChatGPT Plus subscribers active during a nine-hour window may have had payment-related details exposed, and other users' chat titles were visible. Among OpenAI's fixes were "redundant checks to ensure the data returned by our Redis cache matches the requesting user." Source: OpenAI, "March 20 ChatGPT outage: Here's what happened".

Cuboid's response guard is that redundant check, built in from the start, and the drill below proves it fires.

## The six checks
| # | Check | What it does | Code |
|---|---|---|---|
| 1 | Key scope | Random 32-byte keys stored as HMAC-SHA256 with a pepper, compared in constant time. A key maps to one store, one company, or the operator. | `internal/auth` |
| 2 | Path check | The id in the path selects within the key's scope. Anything else returns 404, identical to an id that does not exist. No other input can name a tenant. | `query/http.go`, `authenticate` |
| 3 | Cache partition | One LRU per store or company, with its own quota. Lookups start from the authorised partition, so one store can neither read nor evict another's entries. | `internal/cache` |
| 4 | Row security | `cuboid_reader` has no `BYPASSRLS`. Each read runs in a transaction beginning with `set_config('cuboid.tenant_ids', ..., true)`. The setting is transaction-local, and an unset value matches no rows. | `store.ReadTx` |
| 5 | Result check | Every result row carries its `tenant_id`, asserted before any row is used. A result without this query's shape is refused too. | `query/service.go`, `read` |
| 6 | Response guard | Every response body and SSE frame is checked against the key's scope. A writer wrapper refuses any body that did not pass through the guard or the error path. | `query/http.go`, `writeData` |

Filter values are checked by shape only, never against the shared dictionary: answering "unknown value" for a category another store uses would reveal that it exists. An unknown value matches no rows, as direct SQL would. `/meta` lists values from the store's own cells, under row security.

## The leak drill
The drill injects faults into Cuboid's own code, runs only in demo mode, and targets only the local stack. It sends mixed load from several stores through the real HTTP stack with real keys, cancelling 20% of requests mid-flight. An answer counts as leaked if it matches another store's data rather than the requester's.

| Drill | Fault injected | Stopped by |
|---|---|---|
| A | The cache is keyed by the query alone, shared across stores | Response guard |
| B | The store client hands a request the previous request's rows (the INC-02 mechanism) | Result check |
| C | The tenant setting is skipped on some reads | Row security returns nothing, and the answer is refused |

Run it from the Isolation page or with `make demo-check`. Results are in `artifacts/demo-check/` and `artifacts/isolation/`.

**What a drill's counts mean:** faults are counted per store query, and blocked answers per response. Viewers sharing one query through request coalescing each get a blocked answer, so blocked can exceed faults.

## Tests
- `bench isolation`: randomized requests across all stores and companies with client cancellations. Each answer is compared with the expected answer for the requester's scope. Out-of-scope and non-existent ids must give byte-identical 404s.
- `bench demo-check`: D9 (one store's key on every endpoint of another), D10 (company scope), D17 (drills A to C).
