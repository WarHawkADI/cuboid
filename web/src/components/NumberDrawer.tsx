import { useEffect, useState } from "react";

import { ApiError, Envelope, post, Segment, VerifyResult } from "../api";
import { clock, ms } from "../format";

export interface NumberRef {
  label: string;
  value: string;
  endpoint: string;
  params: Record<string, string>;
  envelope: Envelope<unknown>;
  scopePath: string;
  apiKey: string;
}

// Blocks are proportional to time span: whole months, days and hours from
// rollup cells, and the thin edges read row by row from the mirror.
function SliceStrip({ env }: { env: Envelope<unknown> }) {
  const plan = env.meta.plan;
  type Block = { from: number; to: number; label: string; cls: string };
  const blocks: Block[] = [
    ...plan.segments.map((s: Segment) => ({
      from: Date.parse(s.from),
      to: Date.parse(s.to),
      label: `${s.buckets} ${s.grain}${s.buckets === 1 ? "" : "s"}`,
      cls: `g-${s.grain}`,
    })),
    ...plan.edges.map((e) => ({ from: Date.parse(e.from), to: Date.parse(e.to), label: "", cls: "edge" })),
  ].sort((a, b) => a.from - b.from);
  if (blocks.length === 0) return <p className="empty">This range holds no time, so nothing is read.</p>;
  const total = blocks.reduce((a, b) => a + (b.to - b.from), 0) || 1;
  return (
    <div>
      <div className="slice" role="img" aria-label={blocks.map((b) => b.label || "edge from the mirror").join(", ")}>
        {blocks.map((b, i) => (
          <div key={i} className={b.cls} style={{ flexGrow: Math.max((b.to - b.from) / total, 0.005) * 100, flexBasis: 0 }} title={b.label || "edge read from the mirror"}>
            {(b.to - b.from) / total > 0.12 ? b.label : ""}
          </div>
        ))}
      </div>
      <p className="label small">
        {plan.segments.map((s) => `${s.buckets} ${s.grain}${s.buckets === 1 ? "" : "s"}`).join(", ") || "No whole buckets"}
        {plan.edges.length > 0 ? `, plus ${plan.edges.length} edge${plan.edges.length === 1 ? "" : "s"} from the mirror` : ""}.
      </p>
    </div>
  );
}

export function NumberDrawer({ num, onClose }: { num: NumberRef; onClose: () => void }) {
  const [result, setResult] = useState<VerifyResult | null>(null);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState("");
  const env = num.envelope;

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const check = async () => {
    setChecking(true);
    setError("");
    setResult(null);
    try {
      const r = await post<Envelope<VerifyResult>>(`${num.scopePath}/verify`, num.apiKey, { endpoint: num.endpoint, params: num.params });
      setResult(r.data);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "The check did not complete.");
    } finally {
      setChecking(false);
    }
  };

  const qs = new URLSearchParams(num.params).toString();
  const curl = `curl -H "Authorization: Bearer ${num.apiKey}" "http://localhost:8080${num.scopePath}/${num.endpoint}?${qs}"`;

  return (
    <>
      <div className="scrim" onClick={onClose} />
      <aside className="drawer" role="dialog" aria-modal="true" aria-label="Where this number comes from">
        <div className="row">
          <h2>Where this number comes from</h2>
          <button className="push" onClick={onClose} autoFocus>
            Close
          </button>
        </div>
        <p>
          <span className="figure">{num.value}</span> <span className="label">{num.label}</span>
        </p>
        <section className="stack">
          <SliceStrip env={env} />
          <table>
            <tbody>
              <tr>
                <td className="label">Rollup</td>
                <td className="mono">{env.meta.plan.rollup ?? "none"}</td>
              </tr>
              <tr>
                <td className="label">Range</td>
                <td className="mono">
                  {env.from} to {env.to}
                </td>
              </tr>
              <tr>
                <td className="label">As of</td>
                <td>
                  {env.freshness.as_of ? clock(env.freshness.as_of) : "not yet"} ({env.freshness.updated}), <span className="mono">{env.freshness.lsn}</span>
                </td>
              </tr>
              <tr>
                <td className="label">Answered</td>
                <td>
                  {ms(env.meta.timing_ms.total)}, cache {env.meta.cache}
                </td>
              </tr>
            </tbody>
          </table>
        </section>
        <section className="stack rule">
          <h3>The API call</h3>
          <pre>{curl}</pre>
        </section>
        <section className="stack rule">
          <button className="primary" onClick={check} disabled={checking}>
            {checking ? "Checking…" : "Check against main database"}
          </button>
          {error && <p className="alarm">{error}</p>}
          {result && (
            <div className="stack">
              <p className={result.exact ? "exact" : "bad"}>
                {result.verdict}: {result.rows_compared} row{result.rows_compared === 1 ? "" : "s"} compared
              </p>
              <p className="label">
                Cuboid {ms(result.cuboid_ms)}, direct {ms(result.direct_ms)}. Fence at <span className="mono">{result.fence_lsn}</span>. {result.r_minus + result.r_plus} transaction
                {result.r_minus + result.r_plus === 1 ? "" : "s"} reconciled.
              </p>
              <div className="scroll-x">
                <table>
                  <thead>
                    <tr>
                      <th>Row</th>
                      <th>Cuboid ({result.measures.join(", ")})</th>
                      <th>Direct</th>
                    </tr>
                  </thead>
                  <tbody>
                    {result.cuboid.slice(0, 12).map((row, i) => (
                      <tr key={i}>
                        <td className="mono">{row.key.join(" ")}</td>
                        <td className="mono">{row.values.join(", ")}</td>
                        <td className="mono">{result.direct[i]?.values.join(", ")}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <h3>The direct SQL</h3>
              <pre>{result.direct_sql}</pre>
            </div>
          )}
        </section>
      </aside>
    </>
  );
}
