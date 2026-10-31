import { useEffect, useMemo, useRef, useState } from "react";
import uPlot from "uplot";

import { ApiError, DemoKey, Envelope, get, Item } from "../api";
import { NumberDrawer, NumberRef } from "../components/NumberDrawer";
import { count, RANGES, rupees } from "../format";

interface Summary {
  orders: number;
  sales: string;
  units: number;
  average_order_value: string;
}
interface Trend {
  points: { t: string; value: number }[];
}
interface ItemsData {
  items: Item[];
}

interface Loaded {
  summary: Envelope<Summary>;
  trend: Envelope<Trend>;
  top: Envelope<ItemsData>;
  status: Envelope<ItemsData>;
  category: Envelope<ItemsData>;
  stores?: Envelope<ItemsData>;
}

function TrendChart({ points }: { points: { t: string; value: number }[] }) {
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
          { stroke: slate, grid: { stroke: rule }, ticks: { stroke: rule }, font },
          { stroke: slate, grid: { stroke: rule }, ticks: { stroke: rule }, font, size: 60 },
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
  return (
    <div ref={host} role="img" aria-label={`Orders per day: ${points.length} days, highest ${peak}`} />
  );
}

function Bars({ items, money, onOpen }: { items: Item[]; money: boolean; onOpen: (it: Item, shown: string) => void }) {
  if (items.length === 0) return <p className="empty">No orders in this range. Try Last 30 days.</p>;
  const peak = Math.max(...items.map((i) => Number(i.value)), 1);
  return (
    <div className="bars">
      {items.map((it) => {
        const shown = money ? rupees(it.value) : count(it.value);
        return (
          <div className="bar-row" key={`${it.tenant_id ?? ""}-${String(it.key)}`}>
            <span className="bar-label" title={it.label}>
              {it.label}
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

export function Dashboard({ id, company, keys }: { id: number; company: boolean; keys: DemoKey[] }) {
  const [range, setRange] = useState("last_7d");
  const [status, setStatus] = useState("");
  const [store, setStore] = useState<number | null>(null);
  const [data, setData] = useState<Loaded | null>(null);
  const [error, setError] = useState("");
  const [open, setOpen] = useState<NumberRef | null>(null);

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
    if (!apiKey) return;
    let alive = true;
    const q = (extra: Record<string, string>) => new URLSearchParams({ ...params, ...extra }).toString();
    const load = async () => {
      try {
        const noStatus = new URLSearchParams({ range }).toString();
        const [summary, trend, top, st, category, stores] = await Promise.all([
          get<Envelope<Summary>>(`${scopePath}/orders/summary?${q({})}`, apiKey),
          get<Envelope<Trend>>(`${scopePath}/orders/trend?${new URLSearchParams({ interval: "day", range: "last_30d", ...(status ? { status } : {}) })}`, apiKey),
          get<Envelope<ItemsData>>(`${scopePath}/products/top?${q({ n: "10", by: "sales" })}`, apiKey),
          get<Envelope<ItemsData>>(`${scopePath}/orders/breakdown?by=status&${noStatus}`, apiKey),
          get<Envelope<ItemsData>>(`${scopePath}/orders/breakdown?${q({ by: "category" })}`, apiKey),
          showStores ? get<Envelope<ItemsData>>(`${scopePath}/orders/breakdown?${q({ by: "store" })}`, apiKey) : Promise.resolve(undefined),
        ]);
        if (alive) {
          setData({ summary, trend, top, status: st, category, stores });
          setError("");
        }
      } catch (e) {
        if (alive) setError(e instanceof ApiError ? e.message : "The numbers could not be loaded.");
      }
    };
    load();
    const t = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [apiKey, scopePath, params, range, status, showStores]);

  const phrase = RANGES.find((r) => r.value === range)?.phrase ?? "";
  const s = data?.summary.data;
  const fresh = data?.summary.freshness;
  const opener = (label: string, value: string, endpoint: string, p: Record<string, string>, envelope: Envelope<unknown>) => () =>
    setOpen({ label, value, endpoint, params: p, envelope, scopePath, apiKey });
  const title = company ? `Company ${id}` : self?.name ?? `Store ${id}`;

  return (
    <main className="page">
      <section className="surface stack">
        <div className="head">
          <h1>{title}</h1>
          {!company && <span className="label">Store {id}</span>}
          <span className="push row">
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
            <select aria-label="Status" value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">All statuses</option>
              {["placed", "shipped", "delivered", "cancelled", "returned"].map((x) => (
                <option key={x} value={x}>
                  {x[0].toUpperCase() + x.slice(1)}
                </option>
              ))}
            </select>
          </span>
        </div>
        {!apiKey && <p className="empty">Waiting for demo keys. Cuboid may still be loading its first snapshot.</p>}
        {error && <p className="alarm">{error}</p>}
        {fresh && <p className={fresh.stale ? "stale" : "label"}>{fresh.updated[0].toUpperCase() + fresh.updated.slice(1)}</p>}
        {s && data && (
          <>
            <p className="summary">
              {count(s.orders)} orders and {rupees(s.sales)} in sales {phrase}.
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
                    <button className="link" onClick={opener(label, v, "orders/summary", params, data.summary)}>
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
      {data && (
        <>
          <section className="surface">
            <h2>Orders per day, last 30 days</h2>
            <TrendChart points={data.trend.data.points} />
          </section>
          <div className="cols">
            <section className="surface stack">
              <h2>Top 10 products by sales</h2>
              <Bars items={data.top.data.items} money onOpen={(it, v) => opener(it.label, v, "products/top", { ...params, n: "10", by: "sales" }, data.top)()} />
            </section>
            <section className="surface stack">
              <h2>Orders by status</h2>
              <Bars items={data.status.data.items} money={false} onOpen={(it, v) => opener(it.label, v, "orders/breakdown", { range, by: "status" }, data.status)()} />
              <h2 className="rule">Orders by category</h2>
              <Bars items={data.category.data.items.slice(0, 8)} money={false} onOpen={(it, v) => opener(it.label, v, "orders/breakdown", { ...params, by: "category" }, data.category)()} />
              {data.stores && (
                <>
                  <h2 className="rule">Orders by store</h2>
                  <Bars items={data.stores.data.items} money={false} onOpen={(it, v) => opener(it.label, v, "orders/breakdown", { ...params, by: "store" }, data.stores!)()} />
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
