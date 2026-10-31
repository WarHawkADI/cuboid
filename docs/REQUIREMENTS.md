# Requirements traceability

Every line of the PRD and the problem deck, the component that serves it, what proves it, and its status. "Native" means the local Postgres stack, since the Docker path has not been run yet.

Status: **Proven** (a run in this project shows it), **Partial** (works, but something the requirement asks for is missing), **Not proven** (no run shows it yet).

## Goals
| PRD line | Component | Proof | Status |
|---|---|---|---|
| Fast: under 1 second | `internal/planner`, `internal/cache` | `bench speed` | See `docs/TARGETS.md` |
| Fresh: within a few minutes | `internal/cdc`, `internal/pipeline` | `bench freshness` | See `docs/TARGETS.md` |
| Safe for the database | Separate store; query path holds no main-database connection | `bench safety` | See `docs/TARGETS.md` |
| Separate tenants | Six checks, `docs/ISOLATION.md` | `bench isolation`, `bench redteam`, D9, D17 | See below |
| Correct | Engine, mirror edges, fenced verify | `bench correctness`, model test | See `docs/TARGETS.md` |
| Scales 10 times | Rollups grow with buckets, not rows | `bench growth` | See `docs/TARGETS.md` |

## Must have
| ID | Requirement | Component | Proof | Status |
|---|---|---|---|---|
| FR-1 | Counts and totals for a time range | `orders/count`, `orders/summary` | D1, D8 | Proven |
| FR-2 | Trend per hour or per day | `orders/trend` | D2 | Proven |
| FR-3 | Top-N list | `products/top`, `customers/top`, `cities/top` | D3; each check proves the pushed-down list against the full result | Proven |
| FR-4 | Breakdown by category | `orders/breakdown` | D4 | Proven |
| FR-5 | Tenant filter on every query | `internal/auth`, row security, guard | D9, D10, D17, `bench isolation` | Partial: 1,000,000-request run not yet done |
| FR-6 | New data shows up, including updates | CDC, engine retractions | D5, D6, D7 | Proven |
| FR-7 | Simple API with a short README of the endpoints | `internal/query`, `api/` | D1-D11 | Partial: README not yet written |

## Bonus
| ID | Requirement | Component | Proof | Status |
|---|---|---|---|---|
| B-1 | How fresh the data is | `freshness` in every response | D11 | Proven |
| B-2 | Rebuild from scratch | `internal/core/rebuild.go` | D12 | Partial: proven at the dev tier; the stream stops during a rebuild from main |
| B-3 | Limit heavy queries per tenant | `internal/limiter` | D13 | Partial: others' p99 rose under flood, past the spec's 10% bar |
| B-4 | New metric by config | `internal/core/reload.go` | D14 | Proven |
| B-5 | Monitoring page | `web/src/views/Monitor.tsx` | D15 for its data | Partial: browser checks and design review are not finished |

## Setup
| PRD line | Component | Proof | Status |
|---|---|---|---|
| 1M records, 10 tenants | `cmd/seed --tier small` | Seeded and measured | Proven |
| 10M records, 100 tenants, a few much bigger | `cmd/seed --tier main` | Largest store 19.3%, smallest 19,278 orders | Proven |
| Organisers' script generates the data | `provided/` is empty | | Not proven: the script has not been received |
| Starts by following the README; Docker Compose a plus | `docker-compose.yml`, `Dockerfile` | | Not proven: Docker Desktop is broken on the build machine, so Compose has never run |

## Problem deck
| Line | Proof | Status |
|---|---|---|
| Dashboard queries return in under a second | `bench speed` | See `docs/TARGETS.md` |
| New or updated orders show up within minutes | `bench freshness` | See `docs/TARGETS.md` |
| Counts and totals, trends, top-N and breakdowns through a simple API | D1-D4 | Proven |
| Each tenant sees only its own data | D9, D10, D17 | Proven at the dev tier; larger runs pending |
| Numbers match a direct database query | `bench correctness` | See `docs/TARGETS.md` |
| INC-02 | Drills A, B and C, `docs/ISOLATION.md` | Proven at the dev tier |
