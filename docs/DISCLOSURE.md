# Disclosure

Hackathon rule 1 asks for every third-party tool, API, dataset and open-source component to be disclosed. Licences below were recorded by hand; check them against each project before relying on this list.

## AI assistance
**Claude Code (Anthropic)** wrote most of this project's code, tests and documents, working from a build specification the team supplied, under the team's direction. Judges should treat this as AI-assisted work.

## Data
No external dataset. `cmd/seed` generates all data. The organisers' data script has not been received; it will be listed here if it is used.

## Go modules (direct dependencies)
| Module | Use | Licence |
|---|---|---|
| `github.com/jackc/pgx/v5` | Postgres driver | MIT |
| `github.com/jackc/pglogrepl` | Logical replication protocol | MIT |
| `golang.org/x/sync` | errgroup, singleflight, semaphore | BSD-3-Clause |
| `golang.org/x/time` | Per-key rate limit | BSD-3-Clause |
| `github.com/hashicorp/golang-lru/v2` | Cache partitions | MPL-2.0 |
| `github.com/fsnotify/fsnotify` | Config file watch | BSD-3-Clause |
| `gopkg.in/yaml.v3` | Config parsing | MIT and Apache-2.0 |
| `pgregory.net/rapid` | Model-based tests only | MPL-2.0 |

## npm packages
| Package | Use | Licence |
|---|---|---|
| `react`, `react-dom` | UI | MIT |
| `react-router-dom` | UI routes | MIT |
| `uplot` | Time-series charts | MIT |
| `@fontsource-variable/atkinson-hyperlegible-next`, `@fontsource/atkinson-hyperlegible-mono` | Self-hosted fonts | OFL-1.1 |
| `vite`, `@vitejs/plugin-react` | Build | MIT |
| `typescript` | Build | Apache-2.0 |
| `playwright` | Browser checks, development only | Apache-2.0 |
| `@axe-core/playwright` | Accessibility checks, development only | MPL-2.0 |

## Tools and platforms
PostgreSQL 16 (PostgreSQL Licence), Go, Node.js, Docker images `postgres:16-alpine`, `golang:1.26-alpine`, `node:22-alpine` and `alpine:3.20`.

## Ideas credited
- View selection: Harinarayan, Rajaraman and Ullman, "Implementing Data Cubes Efficiently" (1996).
- Chunked snapshots with watermarks, as an alternative: Netflix's DBLog.
- The INC-02 account: OpenAI, "March 20 ChatGPT outage: Here's what happened".
