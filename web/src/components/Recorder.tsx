import { useEffect, useRef } from "react";
import uPlot from "uplot";

export interface RecPoint {
  t: number;
  app_p50: number;
  app_p95: number;
  main_active: number;
  mode: string;
}

export interface Note {
  t: number;
  text: string;
}

function token(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

const bandToken: Record<string, string> = {
  direct: "--band-direct",
  "direct-cache": "--band-cache",
  cuboid: "--band-cuboid",
};

const bandName: Record<string, string> = {
  direct: "Direct",
  "direct-cache": "Direct + cache",
  cuboid: "Cuboid",
};

const WINDOW_MS = 5 * 60 * 1000;
const HEIGHT = 320;
const MAX_NOTES = 4; // rows of annotation text; older events stay in the page's history

// The recorder: the store app's own query time on a log scale, so the target
// lines stay readable next to a 400 ms spike. The PRD's lines are measured from
// the baseline, the dashboards' path shows as bands, and events are marked where
// they happened. Its right edge is now.
export function Recorder({ points, notes, baseline }: { points: RecPoint[]; notes: Note[]; baseline: number }) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const state = useRef({ points, notes, baseline });

  const end = points.length ? points[points.length - 1].t : Date.now();
  const visible = points.filter((p) => p.t >= end - WINDOW_MS);
  state.current = { points: visible, notes: notes.filter((n) => n.t >= end - WINDOW_MS), baseline };

  useEffect(() => {
    if (!host.current) return;
    const ink = token("--ink");
    const slate = token("--slate");
    const rule = token("--rule");
    const pen = token("--pen");
    const limit = token("--limit");
    const amber = token("--amber");
    const ui = token("--ui");
    const font = `14px ${ui}`;
    const dpr = () => devicePixelRatio;
    const hline = (u: uPlot, value: number, colour: string) => {
      const y = u.valToPos(value, "y", true);
      const c = u.ctx;
      c.save();
      c.strokeStyle = colour;
      c.lineWidth = 1.5 * dpr();
      c.setLineDash([6 * dpr(), 4 * dpr()]);
      c.beginPath();
      c.moveTo(u.bbox.left, y);
      c.lineTo(u.bbox.left + u.bbox.width, y);
      c.stroke();
      c.restore();
      return y;
    };
    const u = new uPlot(
      {
        width: host.current.clientWidth,
        height: HEIGHT,
        legend: { show: false },
        cursor: { show: false },
        scales: {
          x: { time: true },
          y: {
            distr: 3,
            log: 10,
            range: (_u, _min, max) => [Math.min(0.5, Math.max(state.current.baseline / 4, 0.1)), Math.max((max ?? 1) * 1.3, 100)],
          },
        },
        axes: [
          {
            stroke: slate,
            grid: { stroke: rule, width: 1 },
            ticks: { stroke: rule },
            font,
            space: 90,
            values: (_u, ts) => ts.map((t) => new Date(t * 1000).toLocaleTimeString("en-GB", { hour12: false })),
          },
          {
            stroke: slate,
            grid: { stroke: rule, width: 1 },
            ticks: { stroke: rule },
            font,
            size: 70,
            values: (_u, s) => s.map((v) => (v == null ? "" : `${v} ms`)),
          },
        ],
        series: [
          {},
          { label: "App query time p95", stroke: pen, width: 2, points: { show: false }, spanGaps: true },
          { label: "App query time p50", stroke: slate, width: 1, points: { show: false }, spanGaps: true },
        ],
        hooks: {
          drawClear: [
            (u) => {
              const { points } = state.current;
              const c = u.ctx;
              const bbox = u.bbox;
              let start = 0;
              const flush = (end: number) => {
                const mode = points[start]?.mode;
                const t = mode ? bandToken[mode] : undefined;
                if (!t) return;
                const a = u.valToPos(points[start].t / 1000, "x", true);
                const b = u.valToPos(points[end].t / 1000, "x", true);
                c.fillStyle = token(t);
                c.fillRect(a, bbox.top, b - a, bbox.height);
                if (b - a > 110 * dpr()) {
                  c.fillStyle = slate;
                  c.font = `${14 * dpr()}px ${ui}`;
                  c.textAlign = "left";
                  c.fillText(bandName[mode], a + 6 * dpr(), bbox.top + bbox.height - 8 * dpr());
                }
              };
              for (let i = 1; i < points.length; i++) {
                if (points[i].mode !== points[start].mode) {
                  flush(i);
                  start = i;
                }
              }
              if (points.length) flush(points.length - 1);
            },
          ],
          draw: [
            (u) => {
              const { baseline, notes } = state.current;
              const c = u.ctx;
              if (baseline > 0) {
                const great = baseline * 1.1;
                const good = baseline * 1.25;
                hline(u, great, amber);
                const y = hline(u, good, limit);
                // One label for both lines: they sit too close on the scale to label apart.
                c.save();
                c.font = `${14 * dpr()}px ${ui}`;
                c.textAlign = "left";
                const x = u.bbox.left + 8 * dpr();
                const a = `Great: 10% slower, ${great.toFixed(1)} ms`;
                const b = `Good: 25% slower, ${good.toFixed(1)} ms`;
                const w = c.measureText(`${a}   `).width;
                const h = 18 * dpr();
                // Paper behind the words, so the trace never runs through them.
                c.fillStyle = token("--paper");
                c.fillRect(x - 4 * dpr(), y - 5 * dpr() - h, w + c.measureText(b).width + 8 * dpr(), h);
                c.fillStyle = amber;
                c.fillText(a, x, y - 10 * dpr());
                c.fillStyle = limit;
                c.fillText(b, x + w, y - 10 * dpr());
                c.restore();
              }
              // The newest events, each on its own row so no two overlap.
              const shown: Note[] = [];
              for (let i = notes.length - 1; i >= 0 && shown.length < MAX_NOTES; i--) {
                if (!shown.some((s) => s.text === notes[i].text)) shown.unshift(notes[i]);
              }
              c.save();
              c.font = `${14 * dpr()}px ${ui}`;
              shown.forEach((n, row) => {
                const x = u.valToPos(n.t / 1000, "x", true);
                if (x < u.bbox.left || x > u.bbox.left + u.bbox.width) return;
                const y = u.bbox.top + (16 + 18 * row) * dpr();
                c.strokeStyle = ink;
                c.lineWidth = dpr();
                c.beginPath();
                c.moveTo(x, u.bbox.top);
                c.lineTo(x, y + 4 * dpr());
                c.stroke();
                const w = c.measureText(n.text).width;
                const left = x + w + 8 * dpr() > u.bbox.left + u.bbox.width;
                c.fillStyle = ink;
                c.textAlign = left ? "right" : "left";
                c.fillText(n.text, left ? x - 5 * dpr() : x + 5 * dpr(), y);
              });
              c.restore();
            },
          ],
        },
      },
      [[], [], []],
      host.current,
    );
    plot.current = u;
    const obs = new ResizeObserver(() => {
      if (host.current) u.setSize({ width: host.current.clientWidth, height: HEIGHT });
    });
    obs.observe(host.current);
    return () => {
      obs.disconnect();
      u.destroy();
      plot.current = null;
    };
  }, []);

  useEffect(() => {
    // A log scale cannot show zero: a second with no samples is a gap, not a value.
    const clean = (v: number) => (v > 0 ? v : null);
    plot.current?.setData([visible.map((p) => p.t / 1000), visible.map((p) => clean(p.app_p95)), visible.map((p) => clean(p.app_p50))]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [points, notes, baseline]);

  return <div className="recorder" ref={host} role="img" aria-label="Store app query time over the last five minutes, with the PRD's target lines" />;
}
