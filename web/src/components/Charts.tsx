import { useEffect, useRef, useState } from "react";
import uPlot from "uplot";

import { Item } from "../api";
import { count } from "../format";

const titleCase = (s: string) => (s ? s[0].toUpperCase() + s.slice(1).replace(/_/g, " ") : s);

function dayLabel(t: string | number): string {
  return new Date(t).toLocaleDateString("en-IN", { day: "numeric", month: "short" });
}

function TrendChart({ points, unit }: { points: { t: string; value: number }[]; unit: string }) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  useEffect(() => {
    if (!host.current) return;
    const css = getComputedStyle(document.documentElement);
    const slate = css.getPropertyValue("--slate").trim();
    const rule = css.getPropertyValue("--rule").trim();
    const font = `14px ${css.getPropertyValue("--ui").trim()}`;
    const u = new uPlot(
      {
        width: host.current.clientWidth,
        height: 220,
        legend: { show: false },
        cursor: { show: false },
        axes: [
          {
            stroke: slate,
            grid: { stroke: rule },
            ticks: { stroke: rule },
            font,
            values: (_u, ts) => ts.map((t) => dayLabel(t * 1000)),
          },
          { stroke: slate, grid: { stroke: rule }, ticks: { stroke: rule }, font, size: 60, values: (_u, vs) => vs.map((v) => v.toLocaleString("en-IN")) },
        ],
        series: [{}, { stroke: css.getPropertyValue("--pen").trim(), width: 2, points: { show: false } }],
        scales: { y: { range: (_u, _min, max) => [0, Math.max(max * 1.1, 1)] } },
      },
      [[], []],
      host.current,
    );
    plot.current = u;
    const obs = new ResizeObserver(() => host.current && u.setSize({ width: host.current.clientWidth, height: 220 }));
    obs.observe(host.current);
    return () => {
      obs.disconnect();
      u.destroy();
    };
  }, []);
  useEffect(() => {
    plot.current?.setData([points.map((p) => Date.parse(p.t) / 1000), points.map((p) => Number(p.value))]);
  }, [points]);
  const peak = points.reduce((m, p) => Math.max(m, Number(p.value)), 0);
  return <div ref={host} role="img" aria-label={`${unit} per day: ${points.length} days, highest ${peak}`} />;
}

// A trend, with the same numbers available as a table for anyone who cannot read the chart.
export function Trend({ title, unit, points }: { title: string; unit: string; points: { t: string; value: number }[] }) {
  const [table, setTable] = useState(false);
  return (
    <>
      <div className="head">
        <h2>{title}</h2>
        <button className="push" aria-pressed={table} onClick={() => setTable(!table)}>
          {table ? "Show chart" : "Show table"}
        </button>
      </div>
      {table ? (
        <div className="scroll-x trend-table">
          <table>
            <thead>
              <tr>
                <th>Day</th>
                <th className="num">{unit}</th>
              </tr>
            </thead>
            <tbody>
              {points.map((p) => (
                <tr key={p.t}>
                  <td>{dayLabel(p.t)}</td>
                  <td className="num">{count(p.value)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <TrendChart points={points} unit={unit} />
      )}
    </>
  );
}

export function Bars({ items, show, empty, onOpen }: { items: Item[]; show: (v: string | number) => string; empty: string; onOpen: (it: Item, shown: string) => void }) {
  if (items.length === 0) return <p className="empty">{empty}</p>;
  const peak = Math.max(...items.map((i) => Number(i.value)), 1);
  return (
    <div className="bars">
      {items.map((it) => {
        const shown = show(it.value);
        return (
          <div className="bar-row" key={`${it.tenant_id ?? ""}-${String(it.key)}`}>
            <span className="bar-label" title={it.label}>
              {titleCase(it.label)}
            </span>
            <div className="bar-track">
              <div className="bar-fill" style={{ width: `${(100 * Number(it.value)) / peak}%` }} />
            </div>
            <span className="bar-value">
              <button className="link" onClick={() => onOpen(it, shown)}>
                {shown}
              </button>
            </span>
          </div>
        );
      })}
    </div>
  );
}
