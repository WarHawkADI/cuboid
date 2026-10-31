import { useEffect, useMemo, useRef, useState } from "react";

import { ApiError, DemoKey, Envelope, get, Item } from "../api";
import { Bars, Trend as TrendPanel } from "../components/Charts";
import { NumberDrawer, NumberRef } from "../components/NumberDrawer";
import { count, RANGES, rupees } from "../format";
import { Alerts } from "./Alerts";

interface Summary {
  orders: number;
  sales: string;
  units: number;
  average_order_value: string;
}
interface TrendData {
  points: { t: string; value: number }[];
}
interface ItemsData {
  items: Item[];
}

// Each widget loads on its own: one slow query must not blank the page.
interface Loaded {
  summary?: Envelope<Summary>;
  trend?: Envelope<TrendData>;
  top?: Envelope<ItemsData>;
  status?: Envelope<ItemsData>;
  category?: Envelope<ItemsData>;
  stores?: Envelope<ItemsData>;
}

const NONE = "No orders in this range. Try Last 30 days.";

export function Dashboard({ id, company, keys }: { id: number; company: boolean; keys: DemoKey[] }) {
  const [range, setRange] = useState("last_7d");
  const [status, setStatus] = useState("");
  const [store, setStore] = useState<number | null>(null);
  const [data, setData] = useState<Loaded>({});
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [open, setOpen] = useState<NumberRef | null>(null);
  const [dataset, setDataset] = useState<"orders" | "alerts">("orders");
  const [before, setBefore] = useState<{ sales: number; orders: number; days: number } | null>(null);

  const latest = useRef<{ from: string; to: string } | null>(null);
  const self = keys.find((k) => k.id === id && k.kind === (company ? "company" : "store"));
  const apiKey = self?.key ?? "";
  // A company owner can look at one store with the company key.
  const scopePath = company && store === null ? `/companies/${id}` : `/tenants/${store ?? id}`;
  const showStores = company && store === null;

  const params = useMemo(() => {
    const p: Record<string, string> = { range };
    if (status) p.status = status;
    return p;
  }, [range, status]);

  useEffect(() => {
    if (!apiKey || dataset !== "orders") return;
    let alive = true;
    const q = (extra: Record<string, string>) => new URLSearchParams({ ...params, ...extra }).toString();
    const noStatus = new URLSearchParams({ range }).toString();
    const paths: [keyof Loaded, string][] = [
      ["summary", `${scopePath}/orders/summary?${q({})}`],
      ["trend", `${scopePath}/orders/trend?${new URLSearchParams({ interval: "day", range: "last_30d", ...(status ? { status } : {}) })}`],
      ["top", `${scopePath}/products/top?${q({ n: "10", by: "sales" })}`],
      ["status", `${scopePath}/orders/breakdown?by=status&${noStatus}`],
      ["category", `${scopePath}/orders/breakdown?${q({ by: "category" })}`],
    ];
    if (showStores) paths.push(["stores", `${scopePath}/orders/breakdown?${q({ by: "store" })}`]);
    let busy = false;
    const load = async () => {
      if (busy) return; // a slow round must not stack up behind itself
      busy = true;
      let failure = "";
      await Promise.all(
        paths.map(async ([name, path]) => {
          try {
            const env = await get<never>(path, apiKey);
            if (alive) setData((d) => ({ ...d, [name]: env }));
          } catch (e) {
            failure = e instanceof ApiError ? e.message : "Some numbers could not be loaded.";
          }
        }),
      );
      busy = false;
      if (alive) {
        setError(failure);
        setLoaded(true);
      }
    };
    setData({});
    setLoaded(false);
    load();
    const t = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [apiKey, scopePath, params, range, status, showStores, dataset]);

  // The same length of time just before this range. Moved once a minute, not
  // every refresh: a sentence of context does not need a query every 2 seconds.
  latest.current = data.summary && range !== "all" ? { from: data.summary.from, to: data.summary.to } : null;
  const hasWindow = latest.current !== null;
  useEffect(() => {
    setBefore(null);
    if (!apiKey || !hasWindow || dataset !== "orders") return;
    let alive = true;
    const load = async () => {
      const cur = latest.current;
      if (!cur) return;
      const from = Date.parse(cur.from);
      const length = Date.parse(cur.to) - from;
      if (!(length > 0)) return;
      const iso = (t: number) => new Date(Math.floor(t / 1000) * 1000).toISOString().replace(".000Z", "Z");
      const p = new URLSearchParams({ from: iso(from - length), to: iso(from), ...(status ? { status } : {}) });
      try {
        const env = await get<Envelope<Summary>>(`${scopePath}/orders/summary?${p}`, apiKey);
        if (alive) setBefore({ sales: Number(env.data.sales), orders: env.data.orders, days: Math.round(length / 86400000) });
      } catch {
        if (alive) setBefore(null); // context only: the range's own numbers still stand
      }
    };
    load();
    const t = setInterval(load, 60000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [apiKey, scopePath, range, status, hasWindow, dataset]);

  const phrase = RANGES.find((r) => r.value === range)?.phrase ?? "";
  const s = data.summary?.data;
  const fresh = data.summary?.freshness;
  const opener = (label: string, value: string, endpoint: string, p: Record<string, string>, envelope: Envelope<unknown>) => () =>
    setOpen({ label, value, endpoint, params: p, envelope, scopePath, apiKey });
  const change = before && s && before.sales > 0 ? (100 * (Number(s.sales) - before.sales)) / before.sales : null;
  const earlier = before && before.days >= 2 ? `the ${before.days} days before` : "the same length of time before";
  const title = company ? `Company ${id}` : self?.name ?? `Store ${id}`;

  return (
    <main className="page">
      <section className="surface stack">
        <div className="head">
          <h1>{title}</h1>
          {!company && <span className="label">Store {id}</span>}
          <span className="push row">
            <span className="segmented" role="group" aria-label="Dataset">
              <button aria-pressed={dataset === "orders"} onClick={() => setDataset("orders")}>
                Orders
              </button>
              <button aria-pressed={dataset === "alerts"} onClick={() => setDataset("alerts")}>
                SOC alerts
              </button>
            </span>
            {company && self?.stores && (
              <select aria-label="Store" value={store ?? ""} onChange={(e) => setStore(e.target.value ? Number(e.target.value) : null)}>
                <option value="">All {self.stores.length} stores</option>
                {self.stores.map((x) => (
                  <option key={x} value={x}>
                    Store {x}
                  </option>
                ))}
              </select>
            )}
            <select aria-label="Range" value={range} onChange={(e) => setRange(e.target.value)}>
              {RANGES.map((r) => (
                <option key={r.value} value={r.value}>
                  {r.label}
                </option>
              ))}
            </select>
            {dataset === "orders" && (
              <select aria-label="Status" value={status} onChange={(e) => setStatus(e.target.value)}>
                <option value="">All statuses</option>
                {["placed", "shipped", "delivered", "cancelled", "returned"].map((x) => (
                  <option key={x} value={x}>
                    {x[0].toUpperCase() + x.slice(1)}
                  </option>
                ))}
              </select>
            )}
          </span>
        </div>
        {!apiKey && <p className="empty">Waiting for demo keys. Cuboid may still be loading its first snapshot.</p>}
        {dataset === "orders" && error && <p className="alarm">{error}</p>}
        {dataset === "orders" && fresh && <p className={fresh.lag_ms > 30000 ? "stale" : "label"}>{fresh.updated[0].toUpperCase() + fresh.updated.slice(1)}</p>}
        {dataset === "orders" && apiKey && !s && !loaded && <p className="empty">Loading the numbers…</p>}
        {dataset === "orders" && s && data.summary && (
          <>
            <p className="summary">
              {count(s.orders)} orders and {rupees(s.sales)} in sales {phrase}
              {change !== null && (Math.abs(change) < 0.5 ? `, about the same as ${earlier}` : `, ${Math.abs(change).toFixed(0)}% ${change > 0 ? "more" : "less"} than ${earlier}`)}.
            </p>
            <div className="figures">
              {(
                [
                  ["Orders", count(s.orders)],
                  ["Sales", rupees(s.sales)],
                  ["Units", count(s.units)],
                  ["Per order", rupees(s.average_order_value)],
                ] as const
              ).map(([label, v]) => (
                <div key={label}>
                  <div className="figure-v">
                    <button className="link" onClick={opener(label, v, "orders/summary", params, data.summary!)}>
                      {v}
                    </button>
                  </div>
                  <div className="label">{label}</div>
                </div>
              ))}
            </div>
          </>
        )}
      </section>
      {dataset === "alerts" && apiKey && <Alerts scopePath={scopePath} apiKey={apiKey} range={range} />}
      {dataset === "orders" && (
        <>
          <section className="surface">{data.trend ? <TrendPanel title="Orders per day, last 30 days" unit="Orders" points={data.trend.data.points} /> : <p className="empty">Loading…</p>}</section>
          <div className="cols">
            <section className="surface stack">
              <h2>Top 10 products by sales</h2>
              {data.top ? <Bars items={data.top.data.items} show={rupees} empty={NONE} onOpen={(it, v) => opener(it.label, v, "products/top", { ...params, n: "10", by: "sales" }, data.top!)()} /> : <p className="empty">Loading…</p>}
            </section>
            <section className="surface stack">
              <h2>Orders by status</h2>
              {data.status ? <Bars items={data.status.data.items} show={count} empty={NONE} onOpen={(it, v) => opener(it.label, v, "orders/breakdown", { range, by: "status" }, data.status!)()} /> : <p className="empty">Loading…</p>}
              <h2 className="rule">Orders by category</h2>
              {data.category ? <Bars items={data.category.data.items.slice(0, 8)} show={count} empty={NONE} onOpen={(it, v) => opener(it.label, v, "orders/breakdown", { ...params, by: "category" }, data.category!)()} /> : <p className="empty">Loading…</p>}
              {data.stores && (
                <>
                  <h2 className="rule">Orders by store</h2>
                  <Bars items={data.stores.data.items} show={count} empty={NONE} onOpen={(it, v) => opener(it.label, v, "orders/breakdown", { ...params, by: "store" }, data.stores!)()} />
                </>
              )}
            </section>
          </div>
        </>
      )}
      {open && <NumberDrawer num={open} onClose={() => setOpen(null)} />}
    </main>
  );
}
