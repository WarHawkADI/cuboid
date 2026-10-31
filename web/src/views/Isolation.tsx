import { useEffect, useState } from "react";

import { admin, ApiError, publicJSON } from "../api";
import { clock, count, ms } from "../format";

interface Layer {
  name: string;
  checked: number;
  blocked: number;
}

interface DrillReport {
  mode: string;
  seconds: number;
  stores: number[];
  requests: number;
  cancelled: number;
  faults_injected: number;
  blocked_by_layer: Record<string, number>;
  blocked: number;
  leaked: number;
  latency: { p50: number; p99: number };
}

interface IsolationState {
  isolation: { layers: Layer[]; drill_mode: string; cross_tenant_blocked: number; last_drill: DrillReport | null };
  security: { at: string; kind: string; severity: string; detail: string }[];
}

const DRILLS = [
  { mode: "A", name: "Shared cache key", body: "Swaps the per-store cache for one keyed by the query alone." },
  { mode: "B", name: "Connection mix-up", body: "Hands a request the previous request's rows, the mechanism behind INC-02." },
  { mode: "C", name: "Missing tenant setting", body: "Forgets the row-security setting on some reads." },
];

export function Isolation() {
  const [state, setState] = useState<IsolationState | null>(null);
  const [running, setRunning] = useState("");
  const [error, setError] = useState("");
  const [report, setReport] = useState<DrillReport | null>(null);

  useEffect(() => {
    let alive = true;
    const tick = () => publicJSON<IsolationState>("/api/monitor/state?since=9999999999999").then((s) => alive && setState(s), () => undefined);
    tick();
    const id = setInterval(tick, 1000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);

  const run = async (mode: string) => {
    setRunning(mode);
    setError("");
    try {
      setReport(await admin<DrillReport>("/admin/drill", { mode, seconds: 8 }));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "The drill did not run.");
    } finally {
      setRunning("");
    }
  };

  const layers = state?.isolation.layers ?? [];
  const security = state?.security ?? [];
  const shown = report ?? state?.isolation.last_drill ?? null;
  return (
    <main className="page">
      <section className="surface stack">
        <h1>Isolation: six checks between every store and every answer</h1>
        <div className="scroll-x">
          <div className="lanes" style={{ minWidth: 760 }}>
            <span />
            {layers.map((l) => (
              <div key={l.name} className={`gate ${l.blocked > 0 ? "has-blocks" : ""}`}>
                <strong>{l.name}</strong>
                <br />
                {count(l.checked)} checked
                <br />
                <span className={l.blocked > 0 ? "blocked" : ""}>{count(l.blocked)} stopped</span>
              </div>
            ))}
            {(shown?.stores ?? [42, 43, 44]).slice(0, 3).map((s, i) => (
              <div key={s} style={{ display: "contents" }}>
                <span className="lane-name">Store {s}</span>
                {layers.map((l) => (
                  <div key={l.name} className={`lane-cell lane-${"abc"[i]}`} />
                ))}
              </div>
            ))}
          </div>
        </div>
      </section>
      <section className="surface stack">
        <h2>Leak drill</h2>
        <p className="label">
          A drill injects a fault into Cuboid's own code and sends mixed load from several stores, cancelling some requests mid-flight. Each answer is compared with what its store should see. It is labelled as a drill and runs only in demo mode, against this local stack.
        </p>
        <div className="thirds">
          {DRILLS.map((d) => (
            <div key={d.mode} className="stack rule">
              <h3>
                Drill {d.mode}: {d.name}
              </h3>
              <p className="label">{d.body}</p>
              <div>
                <button className="primary" disabled={running !== ""} onClick={() => run(d.mode)}>
                  {running === d.mode ? "Running…" : "Run leak drill"}
                </button>
              </div>
            </div>
          ))}
        </div>
        {error && <p className="alarm">{error}</p>}
        {shown && (
          <div className="stack rule">
            <p className="outcome">
              {count(shown.faults_injected)} faults injected, {count(shown.blocked)} answers blocked, <span className={shown.leaked ? "bad" : "exact"}>{count(shown.leaked)} leaked</span>.
            </p>
            <p className="label">
              Drill {shown.mode}: {count(shown.requests)} requests in {shown.seconds.toFixed(1)} s, {count(shown.cancelled)} cancelled mid-flight, p50 {ms(shown.latency.p50)}, p99 {ms(shown.latency.p99)}.{" "}
              {Object.entries(shown.blocked_by_layer)
                .map(([k, v]) => `${k}: ${count(v)}`)
                .join("; ")}
            </p>
          </div>
        )}
      </section>
      <section className="surface stack">
        <h2>Security events</h2>
        {state && security.length === 0 && <p className="empty">None recorded in this run.</p>}
        <div className="scroll-x">
          <table>
            <tbody>
              {security
                .slice(-10)
                .reverse()
                .map((e, i) => (
                  <tr key={i}>
                    <td>{clock(e.at)}</td>
                    <td>{e.severity}</td>
                    <td className="mono">{e.kind}</td>
                    <td>{e.detail}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      </section>
    </main>
  );
}
