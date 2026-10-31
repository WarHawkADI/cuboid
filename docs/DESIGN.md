# Cuboid design

## Concept
Cuboid's screens look like a calm measuring instrument for one job. The hero is a recorder: a live chart of the store app's own query time with the PRD's target lines across it, so the demo draws its own proof. The signature detail is the slice: open any number and a strip shows how it was assembled from months, days, hours and two thin edges from the mirror.

## The 5-second message of each screen
| Screen | Message |
|---|---|
| Monitoring | The main database is safe under this load, and here is every PRD target. |
| Store dashboard | How my store did in this period compared with the one before, and it is live. |
| Company view | All my stores together and each one separately. |
| Isolation | No store's answer can reach another store, and the drill just proved it. |
| Results | What we measured, including what we did not hit. |

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

## How this was reviewed
One direction was built, then reviewed by looking at screenshots of every view, on desktop and phone, after each change that mattered. `make critique` makes the blurred and half-size copies and a contact sheet at `artifacts/screenshots/index.html`. `web/e2e/shots.mjs` checks each view for browser errors, serious accessibility problems and horizontal overflow.

Two steps the build specification asked for were left out, and are not claimed: exploring three directions before choosing, and 5-second tests with reviewers who had not seen the project.

### Removed accessories
- **Store-coloured lanes on the isolation page.** Three coloured bars showed nothing that changed. A single row now runs to the check that stopped the fault and says how many it stopped. The three store colours went with them.
- **The one-line limiter summary on the monitoring page.** A short table of the busiest stores says more, and a sentence replaces it when nothing has been limited.

### Tried and rejected
- **A linear scale on the recorder.** A 400 ms spike flattened the target lines into one, so the recorder uses a log scale.
- **One label per target line.** They sit too close on the scale to label apart, so one label names both, in each line's colour, on paper so the trace never runs through it.
- **A sliding window as long as the session.** After a long run, recent changes were too narrow to read. The recorder shows 5 minutes.
- **Blanking the page while a slow query loads.** Each widget now loads on its own.
- **Dropping the mode band when the load generator is busy.** That made bands flicker and polluted the baseline, so the recorder keeps the last known state.
