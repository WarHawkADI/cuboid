# Cuboid design

## Concept
Cuboid's screens look like a calm measuring instrument for one job. The hero is a recorder: a live chart of the store app's own query time with the PRD's target lines across it, so the demo draws its own proof. The signature detail is the slice: open any number and a strip shows how it was assembled from months, days, hours and two thin edges from the mirror.

## The 5-second message of each screen
| Screen | Message |
|---|---|
| Monitoring | The main database is safe under this load, and here is every PRD target. |
| Store dashboard | How my store did in this period, and it is live. |
| Company view | All my stores together and each one separately. |
| Isolation | No store's answer can reach another store, and the drill just proved it. |
| Benchmarks | The same workload on each path, with the raw numbers. |

## Colour: each colour has one job
Tokens live only in `web/src/tokens.css`; `tools/contrast` computes the ratios below.

| Name | Hex | Job |
|---|---|---|
| Canvas | `#EEF1F4` | app background |
| Paper | `#FFFFFF` | surfaces and chart areas |
| Ink | `#16202A` | primary text, axes |
| Slate | `#56616E` | secondary text, units |
| Rule | `#D6DCE3` | dividers, minor grid |
| Grid | `#BAC3CD` | major grid |
| Pen blue | `#2846C4` | Cuboid's traces, primary actions, focus ring, the slice |
| Umber | `#8A5A2B` | the Direct path, in comparisons only |
| Limit red | `#C8222B` | breaches, blocked responses, destructive actions |
| Amber | `#A86400` | stale, limited, degraded (always with a word) |
| Verified green | `#1D7A47` | exact-match and Great verdicts only |
| Store A, B, C | `#6B3FA0`, `#0F7C8A`, `#B0446E` | isolation view only |

| Foreground | Background | Ratio | Minimum | Use |
|---|---|---|---|---|
| ink | paper | 16.48 | 4.5 | primary text |
| ink | canvas | 14.54 | 4.5 | primary text on canvas |
| slate | paper | 6.30 | 4.5 | secondary text |
| slate | canvas | 5.56 | 4.5 | secondary text on canvas |
| pen | paper | 7.60 | 4.5 | Cuboid traces and links |
| pen | canvas | 6.70 | 4.5 | links on canvas |
| paper | pen | 7.60 | 4.5 | text on primary buttons |
| umber | paper | 5.87 | 4.5 | Direct path label |
| limit | paper | 5.65 | 4.5 | breach and blocked text |
| amber | paper | 4.68 | 4.5 | stale and limited text |
| verified | paper | 5.35 | 4.5 | exact and Great text |
| storeA | paper | 7.38 | 3.0 | tenant lane A |
| storeB | paper | 4.92 | 3.0 | tenant lane B |
| storeC | paper | 5.39 | 3.0 | tenant lane C |

## Type
Atkinson Hyperlegible Next for interface text and numbers, chosen because its letterforms are unambiguous (0/O, 1/l/I). Atkinson Hyperlegible Mono for SQL, LSNs, paths and key prefixes, and for numeric table columns if the UI face lacks `tnum`. Scale ratio 1.25: 13, 15, 16, 20, 25, 31, 39 px. Sentence case everywhere.

## Shape and depth
Content sits flat on paper, separated by space and rules. Only the drawer has elevation. Radius by role: drawer 12 px, controls 6 px, chips fully rounded, charts and the slice strip 0.

## Motion
| Animation | Trigger | Purpose |
|---|---|---|
| Recorder advances | each second's sample | it is the instrument |
| Drawer slides in, 160 ms | opening a number | connects the panel to the number |
| Verification row resolves | a check finishes | shows the result arrived |
| Progress bar fills | a rebuild runs | shows progress |

`prefers-reduced-motion` removes the drawer slide. There are no entrance animations, hover lifts or looping effects.

## Glossary
store (tenant in the API), company, rollup, cell, grain, slice, edge, fence, freshness. The store view uses the PRD's words (Orders, Sales, Units, Per order). The operator view uses engineering words (query time, data lag, retained WAL).

## Status of the process steps
The three-direction exploration and screenshot critique (14.8 steps 1a and 4) need a running stack and a browser. They are recorded below once done, and are not claimed until then.

### Directions explored
Pending.

### Removed accessories
Pending.

### Tried and rejected
Pending.
