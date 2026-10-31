import { useCallback, useEffect, useRef, useState } from "react";

import { admin, ApiError, publicJSON } from "../api";
import { Note, Recorder, RecPoint } from "../components/Recorder";
import { Autopilot } from "../components/Autopilot";
import { bytes, clock, count, ms } from "../format";

interface Percentile {
  name: string;
  count: number;
  p50: number;
  p95: number;
  p99: number;
}

export interface MonitorState {
  now: number;
  state: string;
  demo_mode: boolean;
  freshness: { lag_ms: number; lsn: string };
  slot: { Exists: boolean; RetainedWAL: number; WALStatus: string };
  pipeline: { applied_lsn?: string; flushes?: number; last_flush_ms?: number };
  alarms: { name: string; detail: string }[];
  latency: Percentile[];
  cache: { hits: number; misses: number; coalesced: number };
  limiter: { partition: string; heavy_in_flight: number; heavy_limit: number; rejected: number }[];
  rebuild: { kind: string; phase: string; done: number; total: number; running: boolean; message: string; error?: string; reason: string };
  verify: {
    checks: number;
    exact: number;
    mismatch: number;
    batch: { running: boolean; total: number; done: number; exact: number; mismatch: number; failed: number };
  };
  reload: { ok: boolean; error?: string; added_rollups?: string[] } | null;
  recorder: { points: (RecPoint & { direct_qps: number; cuboid_qps: number; viewers: number })[]; annotations: Note[]; baseline_p95: number };
}

interface ScoreRow {
  id: string;
  name: string;
  good: string;
  great: string;
  measured: string;
  verdict: string;
  command: string;
}

interface Scorecard {
  available?: boolean;
  generated_at?: string;
  tier?: string;
  rows?: ScoreRow[];
}

const PATHS = [
  { mode: "direct", label: "Direct" },
  { mode: "direct-cache", label: "Direct + cache" },
  { mode: "cuboid", label: "Cuboid" },
  { mode: "off", label: "No dashboards" },
];

export function Monitor() {
  const [state, setState] = useState<MonitorState | null>(null);
  const [points, setPoints] = useState<MonitorState["recorder"]["points"]>([]);
  const [notes, setNotes] = useState<Note[]>([]);
  const [score, setScore] = useState<Scorecard>({});
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [pilot, setPilot] = useState(false);
  const since = useRef(0);

  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const s = await publicJSON<MonitorState>(`/api/monitor/state?since=${since.current}`);
        if (!alive) return;
        setState(s);
        setError("");
        if (s.recorder.points?.length) {
          since.current = s.recorder.points[s.recorder.points.length - 1].t;
          setPoints((p) => [...p, ...s.recorder.points].slice(-900));
        }
        if (s.recorder.annotations?.length) setNotes((n) => [...n, ...s.recorder.annotations].slice(-60));
      } catch {
        if (alive) setError("Cuboid is not answering. It may be restarting; this page keeps trying.");
      }
    };
    tick();
    const id = setInterval(tick, 1000);
    const loadScore = () => publicJSON<Scorecard>("/api/scorecard").then((s) => alive && setScore(s), () => undefined);
    loadScore();
    const sid = setInterval(loadScore, 30000);
    return () => {
      alive = false;
      clearInterval(id);
      clearInterval(sid);
    };
  }, []);

  const act = useCallback(async (label: string, path: string, body?: unknown) => {
    setMessage(`${label}…`);
    try {
      await admin(path, body);
      setMessage(`${label}: done`);
    } catch (e) {
      setMessage(`${label}: ${e instanceof ApiError ? e.message : "failed"}`);
    }
  }, []);

  const last = points[points.length - 1];
  const mode = last?.mode ?? "off";
  const baseline = state?.recorder.baseline_p95 ?? 0;
  const slower = baseline > 0 && last ? ((last.app_p95 - baseline) / baseline) * 100 : null;
  const latency = state?.latency ?? [];
  const hitRate = state && state.cache.hits + state.cache.misses > 0 ? (100 * state.cache.hits) / (state.cache.hits + state.cache.misses) : null;
  const busiest = state?.limiter[0];

  return (
    <main className="page" style={pilot ? { paddingBottom: 170 } : undefined}>
      {error && <p className="alarm">{error}</p>}
      {state?.alarms.map((a) => (
        <p className="alarm" key={a.name}>
          {a.detail}
        </p>
      ))}
      <section className="surface" aria-labelledby="rec-title">
        <div className="head">
          <h1 id="rec-title">Main database, live: normal app query time</h1>
          <div className="segmented push" role="group" aria-label="Dashboards' path">
            {PATHS.map((p) => (
              <button key={p.mode} aria-pressed={mode === p.mode} onClick={() => act(`Switch to ${p.label}`, "/admin/path", { mode: p.mode, viewers: 1000 })}>
                {p.label}
              </button>
            ))}
          </div>
          <button onClick={() => setPilot(true)}>Run demo</button>
        </div>
        <div className="recorder-readout">
          <span>
            <span className="figure">{last ? ms(last.app_p95) : "…"}</span> <span className="label">app p95 now</span>
          </span>
          <span className="label">Baseline {baseline ? ms(baseline) : "not measured yet: run with no dashboards"}</span>
          {slower !== null && (
            <span className={slower < 10 ? "great" : slower < 25 ? "good" : "bad"}>
              {slower <= 0 ? "No slower than baseline" : `${slower.toFixed(1)}% slower than baseline`}
            </span>
          )}
          <span className="label">
            {last ? `${last.viewers} dashboards; ${Math.round(last.direct_qps)} queries/s on the main database; ${Math.round(last.cuboid_qps)} queries/s through Cuboid` : ""}
          </span>
        </div>
        <Recorder points={points} notes={notes} baseline={baseline} />
      </section>

      <div className="cols">
        <section className="surface" aria-labelledby="targets-title">
          <h2 id="targets-title">The PRD's targets</h2>
          {score.rows ? (
            <>
              <div className="scroll-x">
                <table>
                  <thead>
                    <tr>
                      <th>What we measure</th>
                      <th>Good</th>
                      <th>Great</th>
                      <th>Measured</th>
                      <th>Verdict</th>
                    </tr>
                  </thead>
                  <tbody>
                    {score.rows.map((r) => (
                      <tr key={r.id} title={r.command}>
                        <td>{r.name}</td>
                        <td>{r.good}</td>
                        <td>{r.great}</td>
                        <td>{r.measured}</td>
                        <td className={r.verdict === "Great" ? "great" : r.verdict === "Good" ? "good" : r.verdict === "Not measured" ? "label" : "bad"}>{r.verdict}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <p className="label small">
                Measured on the {score.tier} tier at {score.generated_at}. Reproduce each row with the command in docs/TARGETS.md.
              </p>
            </>
          ) : (
            <p className="empty">Not measured yet. Run make targets; every value here comes from that artifact.</p>
          )}
        </section>

        <section className="surface stack" aria-labelledby="now-title">
          <h2 id="now-title">Right now</h2>
          <div className="row">
            <span>
              Data lag <strong>{state && state.freshness.lag_ms >= 0 ? ms(state.freshness.lag_ms) : "…"}</strong>
            </span>
            <span>
              Slot holds <strong>{state ? bytes(state.slot.RetainedWAL) : "…"}</strong> of WAL
            </span>
            <span>
              Cache <strong>{hitRate === null ? "no queries yet" : `${hitRate.toFixed(0)}% hits`}</strong>
            </span>
            <span>
              Limiter <strong>{busiest ? `${busiest.partition}, ${count(busiest.rejected)} limited` : "idle"}</strong>
            </span>
          </div>
          <div>
            Checks against main database:{" "}
            <strong className={state?.verify.mismatch ? "bad" : "exact"}>
              {state ? `${count(state.verify.exact)} of ${count(state.verify.checks)} exact` : "…"}
            </strong>
          </div>
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>Query time, last minute</th>
                  <th className="num">p50</th>
                  <th className="num">p95</th>
                  <th className="num">p99</th>
                  <th className="num">Requests</th>
                </tr>
              </thead>
              <tbody>
                {latency.map((l) => (
                  <tr key={l.name}>
                    <td className="mono">{l.name}</td>
                    <td className="num">{ms(l.p50)}</td>
                    <td className="num">{ms(l.p95)}</td>
                    <td className="num">{ms(l.p99)}</td>
                    <td className="num">{count(l.count)}</td>
                  </tr>
                ))}
                {state && latency.length === 0 && (
                  <tr>
                    <td colSpan={5} className="empty">
                      No queries in the last minute. Switch to Cuboid above.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
          <p className="label small">
            Layer state: {state?.state ?? "…"}. Applied to <span className="mono">{state?.freshness.lsn ?? "…"}</span>.
          </p>
        </section>
      </div>

      <section className="surface stack" aria-labelledby="controls-title">
        <h2 id="controls-title">Controls</h2>
        <div className="row">
          <button className="primary" onClick={() => act("Verify everything", "/admin/verify-batch?n=400")}>
            Verify everything
          </button>
          <button onClick={() => act("Rebuild from the mirror", "/admin/rebuild?source=mirror&scope=all")}>Rebuild from the mirror</button>
          <button onClick={() => act("Rebuild from scratch", "/admin/rebuild?source=main&scope=all")}>Rebuild from scratch</button>
          <button onClick={() => act("Reload config", "/admin/config/reload")}>Reload config</button>
          <button onClick={() => act("Flood heavy queries", "/admin/flood", { seconds: 8 })}>Flood heavy queries</button>
          <button onClick={() => act("Bulk update", "/admin/scenario", { name: "bulk_update" })}>Bulk-update 50,000 orders</button>
          <button onClick={() => act("Drop replication", "/admin/chaos", { name: "drop_replication" })}>Drop replication</button>
          <button className="danger" onClick={() => act("Restart the layer", "/admin/chaos", { name: "kill" })}>
            Kill Cuboid
          </button>
          <button className="danger" onClick={() => act("Truncate and reload", "/admin/scenario", { name: "truncate_reload" })}>
            Truncate and reload
          </button>
        </div>
        {message && (
          <p className="label" role="status">
            {message}
          </p>
        )}
        {state?.verify.batch.total ? (
          <p>
            Verify everything: {count(state.verify.batch.done)} of {count(state.verify.batch.total)} run, <span className="exact">{count(state.verify.batch.exact)} exact</span>
            {state.verify.batch.mismatch > 0 && <span className="bad">, {state.verify.batch.mismatch} mismatched</span>}
            {state.verify.batch.failed > 0 && <span className="stale">, {state.verify.batch.failed} did not complete</span>}
          </p>
        ) : null}
        {state?.rebuild.phase && (
          <div className="stack">
            <p>
              {state.rebuild.running ? `Rebuilding from the ${state.rebuild.kind}: ${state.rebuild.phase}` : state.rebuild.error ? `Rebuild failed: ${state.rebuild.error}` : state.rebuild.message}
            </p>
            {state.rebuild.running && state.rebuild.total > 0 && (
              <div className="progress" role="progressbar" aria-valuenow={state.rebuild.done} aria-valuemax={state.rebuild.total}>
                <div className="progress-fill" style={{ width: `${(100 * state.rebuild.done) / state.rebuild.total}%` }} />
              </div>
            )}
          </div>
        )}
        {state?.reload && !state.reload.ok && <p className="alarm">Config not applied: {state.reload.error}</p>}
      </section>
      <p className="label small">Page updated {state ? clock(state.now) : "…"}.</p>
      {pilot && <Autopilot onClose={() => setPilot(false)} />}
    </main>
  );
}
