import { useEffect, useRef } from "react";
import uPlot from "uplot";

export interface RecPoint {
  t: number;
  app_p50: number;
  app_p95: number;
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

// The recorder: the store app's own query time, with the PRD's target lines
// measured from the baseline, the dashboards' path as bands, and events marked
// where they happened. Its right edge is now.
export function Recorder({ points, notes, baseline }: { points: RecPoint[]; notes: Note[]; baseline: number }) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const state = useRef({ points, notes, baseline });
  state.current = { points, notes, baseline };

  useEffect(() => {
    if (!host.current) return;
    const ink = token("--ink");
    const slate = token("--slate");
    const rule = token("--rule");
    const pen = token("--pen");
    const limit = token("--limit");
    const amber = token("--amber");
    const font = `14px ${token("--ui")}`;
    const line = (u: uPlot, value: number, colour: string, label: string) => {
      const y = u.valToPos(value, "y", true);
      if (y < u.bbox.top || y > u.bbox.top + u.bbox.height) return;
      const c = u.ctx;
      c.save();
      c.strokeStyle = colour;
      c.lineWidth = 1.5 * devicePixelRatio;
      c.setLineDash([6 * devicePixelRatio, 4 * devicePixelRatio]);
      c.beginPath();
      c.moveTo(u.bbox.left, y);
      c.lineTo(u.bbox.left + u.bbox.width, y);
      c.stroke();
      c.setLineDash([]);
      c.fillStyle = colour;
      c.font = `${14 * devicePixelRatio}px ${token("--ui")}`;
      c.textAlign = "right";
      c.fillText(label, u.bbox.left + u.bbox.width - 6, y - 5 * devicePixelRatio);
      c.restore();
    };
    const u = new uPlot(
      {
        width: host.current.clientWidth,
        height: 320,
        legend: { show: false },
        cursor: { show: false },
        scales: {
          x: { time: true },
          y: {
            range: (_u, _min, max) => {
              const b = state.current.baseline;
              return [0, Math.max(max * 1.15, b * 1.5, 1)];
            },
          },
        },
        axes: [
          { stroke: slate, grid: { stroke: rule, width: 1 }, ticks: { stroke: rule }, font },
          { stroke: slate, grid: { stroke: rule, width: 1 }, ticks: { stroke: rule }, font, size: 64, values: (_u, s) => s.map((v) => `${v} ms`) },
        ],
        series: [
          {},
          { label: "App query time p95", stroke: pen, width: 2, points: { show: false } },
          { label: "App query time p50", stroke: slate, width: 1, points: { show: false } },
        ],
        hooks: {
          drawClear: [
            (u) => {
              const { points } = state.current;
              const c = u.ctx;
              let start = 0;
              const bbox = u.bbox;
              const flush = (end: number) => {
                const mode = points[start]?.mode;
                const t = mode ? bandToken[mode] : undefined;
                if (!t) return;
                const a = u.valToPos(points[start].t / 1000, "x", true);
                const b = u.valToPos(points[end].t / 1000, "x", true);
                c.fillStyle = token(t);
                c.fillRect(a, bbox.top, b - a, bbox.height);
                c.fillStyle = slate;
                c.font = `${14 * devicePixelRatio}px ${token("--ui")}`;
                c.textAlign = "left";
                if (b - a > 110 * devicePixelRatio) c.fillText(bandName[mode], a + 6, bbox.top + bbox.height - 8 * devicePixelRatio);
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
              if (baseline > 0) {
                line(u, baseline * 1.1, amber, `Great: 10% slower ${(baseline * 1.1).toFixed(1)} ms`);
                line(u, baseline * 1.25, limit, `Good: 25% slower ${(baseline * 1.25).toFixed(1)} ms`);
              }
              const c = u.ctx;
              c.save();
              c.font = `${14 * devicePixelRatio}px ${token("--ui")}`;
              let row = 0;
              for (const n of notes.slice(-8)) {
                const x = u.valToPos(n.t / 1000, "x", true);
                if (x < u.bbox.left || x > u.bbox.left + u.bbox.width) continue;
                c.strokeStyle = ink;
                c.lineWidth = devicePixelRatio;
                const y = u.bbox.top + (18 + 18 * (row % 4)) * devicePixelRatio;
                c.beginPath();
                c.moveTo(x, u.bbox.top);
                c.lineTo(x, y);
                c.stroke();
                c.fillStyle = slate;
                const w = c.measureText(n.text).width;
                const left = x + w + 8 > u.bbox.left + u.bbox.width;
                c.textAlign = left ? "right" : "left";
                c.fillText(n.text, left ? x - 4 : x + 4, y);
                row++;
              }
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
      if (host.current) u.setSize({ width: host.current.clientWidth, height: 320 });
    });
    obs.observe(host.current);
    return () => {
      obs.disconnect();
      u.destroy();
      plot.current = null;
    };
  }, []);

  useEffect(() => {
    plot.current?.setData([points.map((p) => p.t / 1000), points.map((p) => p.app_p95), points.map((p) => p.app_p50)]);
  }, [points, notes, baseline]);

  return <div className="recorder" ref={host} role="img" aria-label="Store app query time over the last minutes, with the PRD's target lines" />;
}
