import { useEffect, useState } from "react";

import { publicJSON } from "../api";
import { ms } from "../format";

interface Step {
  mode: string;
  viewers: number;
  app_p50_ms: number;
  app_p95_ms: number;
  slower_pct: number;
  dashboard_p99_ms: number;
  dashboard_errors: number;
  tripped?: boolean;
}

interface BenchData {
  available?: boolean;
  generated_at?: string;
  tier?: string;
  environment?: Record<string, string>;
  steps?: Step[];
}

const colour: Record<string, string> = { direct: "var(--umber)", "direct-cache": "var(--amber)", cuboid: "var(--pen)", "app-alone": "var(--slate)", "cuboid-no-dashboards": "var(--slate)" };

function Chart({ title, unit, steps, value }: { title: string; unit: string; steps: Step[]; value: (s: Step) => number }) {
  const W = 440;
  const H = 220;
  const P = { l: 56, r: 92, t: 12, b: 34 };
  const modes = [...new Set(steps.map((s) => s.mode))].filter((m) => steps.filter((s) => s.mode === m).length > 1);
  const xs = [...new Set(steps.map((s) => s.viewers))].sort((a, b) => a - b);
  const maxY = Math.max(...steps.map(value), 1);
  const x = (v: number) => P.l + (xs.indexOf(v) / Math.max(xs.length - 1, 1)) * (W - P.l - P.r);
  const y = (v: number) => H - P.b - (v / maxY) * (H - P.t - P.b);
  return (
    <figure style={{ margin: 0 }}>
      <figcaption>
        <h3>{title}</h3>
      </figcaption>
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" role="img" aria-label={title}>
        <line x1={P.l} x2={W - P.r} y1={H - P.b} y2={H - P.b} stroke="var(--grid)" />
        <text x={P.l - 6} y={y(maxY) + 5} textAnchor="end" fontSize="13" fill="var(--slate)">
          {maxY.toFixed(0)}
        </text>
        <text x={P.l - 6} y={H - P.b} textAnchor="end" fontSize="13" fill="var(--slate)">
          0 {unit}
        </text>
        {xs.map((v) => (
          <text key={v} x={x(v)} y={H - 12} textAnchor="middle" fontSize="13" fill="var(--slate)">
            {v}
          </text>
        ))}
        {modes.map((m) => {
          const pts = steps.filter((s) => s.mode === m).sort((a, b) => a.viewers - b.viewers);
          const lastPt = pts[pts.length - 1];
          return (
            <g key={m}>
              <polyline fill="none" stroke={colour[m] ?? "var(--ink)"} strokeWidth="2" points={pts.map((p) => `${x(p.viewers)},${y(value(p))}`).join(" ")} />
              <text x={x(lastPt.viewers) + 6} y={y(value(lastPt)) + 5} fontSize="13" fill={colour[m] ?? "var(--ink)"}>
                {m}
              </text>
            </g>
          );
        })}
      </svg>
    </figure>
  );
}

export function Bench() {
  const [data, setData] = useState<BenchData | null>(null);
  useEffect(() => {
    publicJSON<BenchData>("/api/bench").then(setData, () => setData({ available: false }));
  }, []);
  if (!data) return <main className="page" />;
  if (!data.steps)
    return (
      <main className="page">
        <section className="surface">
          <h1>Benchmarks</h1>
          <p className="empty">Not measured yet. Run make bench; this page draws only from that artifact.</p>
        </section>
      </main>
    );
  const steps = data.steps;
  return (
    <main className="page">
      <section className="surface stack">
        <h1>The same workload on each path</h1>
        <p className="label">
          {data.tier} tier, measured {data.generated_at}. Viewers on the horizontal axis.
        </p>
        <div className="multiples">
          <Chart title="App query time p95" unit="ms" steps={steps} value={(s) => s.app_p95_ms} />
          <Chart title="Dashboard query time p99" unit="ms" steps={steps} value={(s) => s.dashboard_p99_ms} />
        </div>
      </section>
      <section className="surface stack">
        <h2>Raw numbers</h2>
        <div className="scroll-x">
          <table>
            <thead>
              <tr>
                <th>Path</th>
                <th className="num">Dashboards</th>
                <th className="num">App p50</th>
                <th className="num">App p95</th>
                <th className="num">Slower than app alone</th>
                <th className="num">Dashboard p99</th>
                <th className="num">Errors</th>
              </tr>
            </thead>
            <tbody>
              {steps.map((s, i) => (
                <tr key={i}>
                  <td>
                    {s.mode}
                    {s.tripped ? " (stopped early)" : ""}
                  </td>
                  <td className="num">{s.viewers}</td>
                  <td className="num">{ms(s.app_p50_ms)}</td>
                  <td className="num">{ms(s.app_p95_ms)}</td>
                  <td className="num">{s.slower_pct.toFixed(1)}%</td>
                  <td className="num">{s.dashboard_p99_ms ? ms(s.dashboard_p99_ms) : "none"}</td>
                  <td className="num">{s.dashboard_errors}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p>
          <a href="/api/bench">Download the raw JSON</a>
        </p>
      </section>
    </main>
  );
}
