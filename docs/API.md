# API

The machine-readable spec is `api/openapi.yaml`. `api/cuboid.postman_collection.json` and `api/requests.http` hold the PRD's examples.

## Access
Send `Authorization: Bearer <key>`. A key belongs to one store, one company (all its stores) or the operator. The id in the path only selects within what the key allows.

| Situation | Answer |
|---|---|
| No key, unknown key, or a key in the URL | 401 |
| A store or company outside the key's scope, or one that does not exist | 404, the same in both cases |
| Unknown parameter, bad value, `n` out of range, `from` after `to` | 400 |
| No rollup covers the query, or too many raw rows would be read | 422, with a hint |
| A store over its query limit, or a key over its rate | 429 with `Retry-After` |
| The aggregate store is busy and no earlier answer exists | 503 with `Retry-After` |

## Endpoints
Everything under `/tenants/{id}` also works under `/companies/{id}`.

| PRD | Endpoint | Parameters |
|---|---|---|
| FR-1 | `GET /tenants/{id}/orders/count` | range, filters |
| FR-1 | `GET /tenants/{id}/orders/summary` | range, filters |
| FR-2 | `GET /tenants/{id}/orders/trend` | `interval=hour\|day\|month`, `metric`, `split_by`, range, filters |
| FR-3 | `GET /tenants/{id}/products/top`, `customers/top`, `cities/top` | `n` (1-100), `by=sales\|units\|orders`, range, filters |
| FR-4 | `GET /tenants/{id}/orders/breakdown` | `by=status\|category\|channel\|city`, `metric`, range, filters; `by=store` under `/companies` |
| B-4 | `GET /tenants/{id}/meta` | What can be asked, generated from config |
| Live | `GET /tenants/{id}/orders/stream?query=orders/count&range=today` | Server-sent events |
| Check | `POST /tenants/{id}/verify` | `{"endpoint": "orders/count", "params": {"range": "last_7d"}}` |

The same shape serves SOC alerts through config: `/tenants/{id}/soc_alerts/count`, `/tenants/{id}/tactics/top`.

Operator routes need the operator key: `/admin/rebuild`, `/admin/config/reload`, `/admin/drill`, `/admin/flood`, `/admin/path`, `/admin/scenario`, `/admin/chaos`, `/admin/verify-batch`, `/admin/targets/run`.

Open routes: `/monitor` (the page), `/api/monitor/state`, `/api/scorecard`, `/metrics`, `/healthz`. The monitoring state shows operational numbers and store ids, not store data. `/api/demo/keys` exists only with `CUBOID_DEMO_MODE=true`.

## Time
- `from` is inclusive and `to` exclusive, as a date or date-time. Without an offset, the value is wall-clock time in the main database's zone, exactly as a SQL literal would be.
- Presets, in that zone: `today`, `yesterday`, `this_week` (ISO week, Monday to now), `last_7d` (now minus 7 x 24 h, to now), `this_month`, `last_30d`, `last_90d`, `this_year`, `all`.
- With no range given, `last_7d` is used.
- Every response echoes the absolute `from` and `to` it used. To compare with your own SQL, use those instants, because presets move with the clock.

## Meanings
- `orders = COUNT(*)`, `sales = SUM(amount)`, `units = SUM(quantity)`, with `created_at >= from AND created_at < to`.
- Filters are equality on a dimension. Join dimensions use the current value, as a direct `JOIN` would. An unknown value matches no rows.
- Money is an exact decimal string such as `"18432.00"`, never a float. Average order value rounds half up.
- Trend buckets are zero-filled. Top-N is ordered value descending, then key ascending. Breakdowns list non-zero values.

## Every response
```json
{
  "tenant_id": 42,
  "query": {"endpoint": "orders/count", "range": "last_7d"},
  "from": "2026-10-24T13:47:20+05:30",
  "to": "2026-10-31T13:47:20+05:30",
  "data": {"count": 724},
  "freshness": {"as_of": "2026-10-31T13:47:20.089+05:30", "lsn": "0/383DCF8", "lag_ms": 200, "updated": "updated just now", "stale": false},
  "meta": {
    "plan": {"rollup": "totals", "segments": [], "edges": [], "cells_estimate": 29, "edge_rows_estimate": 6},
    "cache": "miss",
    "exact": true,
    "heavy": false,
    "timing_ms": {"total": 1.96, "store": 1.96}
  }
}
```

`meta.plan.top_bound` appears on a top-N list that was found by walking month totals instead of ranking every key (ADR-011): `{"rollup": "customer_month", "months_from_cells": 2, "slices_from_mirror": 2, "depth": 640, "keys_read": 1280}`. `keys_read` is how many keys it summed exactly. The answer is the same as ranking every key. When it is absent, every key was ranked.

`meta.cache` is `hit`, `miss`, `coalesced` (shared another request's computation) or `stale`. A `stale` answer is the newest held, exact as of its `as_of`, with `freshness.stale` set to true. It is served for up to 2 seconds while a refresh runs, or for longer if the store cannot answer in time.
