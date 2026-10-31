import { useEffect, useMemo, useState } from "react";

import { ApiError, Envelope, get, Item } from "../api";
import { Bars, Trend } from "../components/Charts";
import { NumberDrawer, NumberRef } from "../components/NumberDrawer";
import { count, RANGES } from "../format";

interface Summary {
  alerts: number;
  risk_total: number;
  average_risk: string;
}
interface Loaded {
  summary?: Envelope<Summary>;
  trend?: Envelope<{ points: { t: string; value: number }[] }>;
  severity?: Envelope<{ items: Item[] }>;
  status?: Envelope<{ items: Item[] }>;
  tactic?: Envelope<{ items: Item[] }>;
  source?: Envelope<{ items: Item[] }>;
}

// Mitigata's own shape of data on the same engine: a store is a client
// organisation, and nothing here is specific to alerts except this page's labels.
export function Alerts({ scopePath, apiKey, range }: { scopePath: string; apiKey: string; range: string }) {
  const [data, setData] = useState<Loaded>({});
  const [error, setError] = useState("");
  const [open, setOpen] = useState<NumberRef | null>(null);

  const paths = useMemo<[keyof Loaded, string, Record<string, string>][]>(
    () => [
      ["summary", "soc_alerts/summary", { range }],
      ["trend", "soc_alerts/trend", { range: "last_30d", interval: "day" }],
      ["severity", "soc_alerts/breakdown", { range, by: "severity" }],
      ["status", "soc_alerts/breakdown", { range, by: "status" }],
      ["tactic", "tactics/top", { range, n: "10", by: "alerts" }],
      ["source", "soc_alerts/breakdown", { range, by: "source_product" }],
    ],
    [range],
  );

  useEffect(() => {
    let alive = true;
    let busy = false;
    const load = async () => {
      if (busy) return;
      busy = true;
      let failure = "";
      await Promise.all(
        paths.map(async ([name, path, p]) => {
          try {
            const env = await get<never>(`${scopePath}/${path}?${new URLSearchParams(p)}`, apiKey);
            if (alive) setData((d) => ({ ...d, [name]: env }));
          } catch (e) {
            failure = e instanceof ApiError ? e.message : "Some numbers could not be loaded.";
          }
        }),
      );
      busy = false;
      if (alive) setError(failure);
    };
    setData({});
    load();
    const t = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [apiKey, scopePath, paths]);

  const phrase = RANGES.find((r) => r.value === range)?.phrase ?? "";
  const s = data.summary?.data;
  const fresh = data.summary?.freshness;
  const panel = (title: string, name: keyof Loaded) => {
    const env = data[name] as Envelope<{ items: Item[] }> | undefined;
    const entry = paths.find((p) => p[0] === name)!;
    return (
      <section className="surface stack">
        <h2>{title}</h2>
        {env ? (
          <Bars
            items={env.data.items.slice(0, 10)}
            show={count}
            empty="No alerts in this range. Try Last 30 days."
            onOpen={(it, v) => setOpen({ label: it.label, value: v, endpoint: entry[1], params: entry[2], envelope: env, scopePath, apiKey })}
          />
        ) : (
          <p className="empty">Loading…</p>
        )}
      </section>
    );
  };

  return (
    <>
      <section className="surface stack">
        {error && <p className="alarm">{error}</p>}
        {fresh && <p className={fresh.lag_ms > 30000 ? "stale" : "label"}>{fresh.updated[0].toUpperCase() + fresh.updated.slice(1)}</p>}
        {s && data.summary ? (
          <>
            <p className="summary">
              {count(s.alerts)} alerts {phrase}, with an average risk score of {s.average_risk}.
            </p>
            <div className="figures">
              {(
                [
                  ["Alerts", count(s.alerts)],
                  ["Total risk", count(s.risk_total)],
                  ["Average risk", s.average_risk],
                ] as const
              ).map(([label, v]) => (
                <div key={label}>
                  <div className="figure-v">
                    <button className="link" onClick={() => setOpen({ label, value: v, endpoint: "soc_alerts/summary", params: { range }, envelope: data.summary!, scopePath, apiKey })}>
                      {v}
                    </button>
                  </div>
                  <div className="label">{label}</div>
                </div>
              ))}
            </div>
            <p className="label small">Synthetic alerts, one client organisation per store. Same engine, isolation and checks as orders, added through config/metrics.yaml only.</p>
          </>
        ) : (
          !error && <p className="empty">Loading the numbers…</p>
        )}
      </section>
      <section className="surface">{data.trend ? <Trend title="Alerts per day, last 30 days" unit="Alerts" points={data.trend.data.points} /> : <p className="empty">Loading…</p>}</section>
      <div className="cols">
        {panel("Alerts by severity", "severity")}
        {panel("Alerts by status", "status")}
        {panel("Top 10 ATT&CK tactics", "tactic")}
        {panel("Alerts by source product", "source")}
      </div>
      {open && <NumberDrawer num={open} onClose={() => setOpen(null)} />}
    </>
  );
}
