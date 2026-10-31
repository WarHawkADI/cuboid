import { useEffect, useState } from "react";

import { publicJSON } from "../api";
import { count, ms } from "../format";

interface Pct {
  count: number;
  p50: number;
  p99: number;
  max: number;
}

interface SpeedRow {
  query: string;
  uncached: Pct;
  cached: Pct;
  largest_store_uncached: Pct;
}

interface Speed {
  tier: string;
  stores: number;
  generated_at: string;
  rows: SpeedRow[];
  viewers: number;
  viewers_p50_ms: number;
  viewers_p99_ms: number;
  viewers_errors: number;
}

interface GrowthRow {
  query: string;
  uncached_p99_ratio: number;
  largest_store_ratio: number;
}

interface Results {
  speed_main?: Speed;
  speed_small?: Speed;
  growth?: { rows: GrowthRow[] };
  freshness?: { inserts?: Pct; insert_to_visible_ms?: Pct; update_to_visible_ms?: Pct; timed_out: number };
  correctness?: { tier: string; quiesced: { total: number; exact: number }; live: { total: number; exact: number } };
  redteam?: { tier: string; rows: { pass: boolean }[] };
  isolation?: { tier: string; requests: number; cross_tenant: number; identical_404: boolean };
  chaos?: { tier: string; kills: number; all_exact: boolean; recovery_max_ms: number };
  demo_check?: { tier: string; rows: { pass: boolean }[] };
}

function Fact({ value, label, tone }: { value: string; label: string; tone?: string }) {
  return (
    <div>
      <div className={`figure-v ${tone ?? ""}`}>{value}</div>
      <div className="label">{label}</div>
    </div>
  );
}

export function Bench() {
  const [r, setR] = useState<Results | null>(null);
  useEffect(() => {
    publicJSON<Results>("/api/results").then(setR, () => setR({}));
  }, []);
  if (!r) return <main className="page" />;
  const speed = r.speed_main;
  const growth = new Map((r.growth?.rows ?? []).map((g) => [g.query, g]));
  const cor = r.correctness;
  const exact = cor ? cor.quiesced.exact + cor.live.exact : 0;
  const total = cor ? cor.quiesced.total + cor.live.total : 0;
  const passed = (rows?: { pass: boolean }[]) => (rows ?? []).filter((x) => x.pass).length;
  const fresh = r.freshness?.insert_to_visible_ms;

  return (
    <main className="page">
      <section className="surface stack">
        <h1>Measured results</h1>
        <p className="label">Every service on one laptop: both databases, Cuboid, the store app and the load generator share its cores and disk. Every number on this page is read from a file in artifacts/.</p>
        <div className="figures">
          {speed && <Fact value={ms(speed.viewers_p99_ms)} label={`Dashboard p99, ${count(speed.viewers)} viewers, ${speed.stores} stores`} />}
          {fresh && <Fact value={ms(fresh.p99)} label="New order to visible, p99" />}
          {cor && <Fact value={`${count(exact)} of ${count(total)}`} label={`Exact against the main database, ${cor.tier} tier`} tone={exact === total ? "exact" : "bad"} />}
          {r.redteam && <Fact value={`${passed(r.redteam.rows)} of ${r.redteam.rows.length}`} label={`Red-team checks, ${r.redteam.tier} tier`} tone={passed(r.redteam.rows) === r.redteam.rows.length ? "exact" : "bad"} />}
        </div>
      </section>

      {speed ? (
        <section className="surface stack">
          <h2>Query time at 10 million orders, {speed.stores} stores</h2>
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>Query</th>
                  <th className="num">Uncached p50</th>
                  <th className="num">Uncached p99</th>
                  <th className="num">Largest store, slowest</th>
                  <th className="num">Cached p99</th>
                  <th className="num">Largest store at 1M vs 10M</th>
                </tr>
              </thead>
              <tbody>
                {speed.rows.map((q) => {
                  const g = growth.get(q.query);
                  return (
                    <tr key={q.query}>
                      <td>{q.query}</td>
                      <td className="num">{ms(q.uncached.p50)}</td>
                      <td className="num">{ms(q.uncached.p99)}</td>
                      <td className={`num ${q.largest_store_uncached.max >= 200 ? "good" : ""}`}>{ms(q.largest_store_uncached.max)}</td>
                      <td className="num">{ms(q.cached.p99)}</td>
                      <td className={`num ${g && g.largest_store_ratio >= 5 ? "bad" : ""}`}>{g ? `${g.largest_store_ratio.toFixed(1)}x` : "not measured"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p className="label small">
            Viewers read from cache, so their p99 stays low even where an uncached query is slower. Top customers grows with the number of customers in the range, which rollups cannot shrink: this is the known limit.
          </p>
        </section>
      ) : (
        <section className="surface">
          <p className="empty">Not measured yet. Run make speed-test.</p>
        </section>
      )}

      <section className="surface stack">
        <h2>Isolation and recovery</h2>
        <div className="scroll-x">
          <table>
            <tbody>
              {r.isolation && (
                <tr>
                  <td>Randomized requests with client cancellations</td>
                  <td>
                    {count(r.isolation.requests)} requests, <span className={r.isolation.cross_tenant === 0 ? "exact" : "bad"}>{count(r.isolation.cross_tenant)} crossed stores</span>
                  </td>
                  <td className="label">{r.isolation.tier} tier</td>
                </tr>
              )}
              {r.isolation && (
                <tr>
                  <td>Another store's id and a missing id</td>
                  <td>{r.isolation.identical_404 ? "Identical 404" : "Different answers"}</td>
                  <td className="label">{r.isolation.tier} tier</td>
                </tr>
              )}
              {r.chaos && (
                <tr>
                  <td>Cuboid killed under writes, kill -9</td>
                  <td>
                    {r.chaos.kills} kills, {r.chaos.all_exact ? <span className="exact">exact after every one</span> : <span className="bad">not all exact</span>}, back within {ms(r.chaos.recovery_max_ms)}
                  </td>
                  <td className="label">{r.chaos.tier} tier</td>
                </tr>
              )}
              {r.demo_check && (
                <tr>
                  <td>Acceptance checks: the PRD's examples</td>
                  <td>
                    {passed(r.demo_check.rows)} of {r.demo_check.rows.length} pass
                  </td>
                  <td className="label">{r.demo_check.tier} tier</td>
                </tr>
              )}
              {r.redteam && (
                <tr>
                  <td>Red-team checks: keys, injection, row security, recategorising, order moves</td>
                  <td>
                    {passed(r.redteam.rows)} of {r.redteam.rows.length} pass
                  </td>
                  <td className="label">{r.redteam.tier} tier</td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </section>

      <section className="surface stack">
        <h2>Not measured</h2>
        <p>
          Database safety at 10 million orders has no final number. A short test with no dashboards showed the app's query time unchanged by Cuboid once settled. It rose under 1,000 dashboards on this laptop, less than on the direct path.
        </p>
      </section>
    </main>
  );
}
