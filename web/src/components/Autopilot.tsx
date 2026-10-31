import { useCallback, useEffect, useRef, useState } from "react";

import { admin, ApiError, DemoKey, Envelope, get, loadKeys, operatorKey, post, VerifyResult } from "../api";
import { count, ms, rupees } from "../format";

interface Beat {
  title: string;
  say: string;
  run: (k: DemoKey[]) => Promise<string>;
}

const STORE = 42;
const OTHER = 43;

function storeKey(keys: DemoKey[], id: number) {
  return keys.find((k) => k.kind === "store" && k.id === id)?.key ?? "";
}

let metricBuild: Promise<{ ok: boolean; took_ms: number; error?: string }> | null = null;

const beats: Beat[] = [
  {
    title: "The problem",
    say: "One big store's report makes the app slow for all stores: the PRD's words. Watch the recorder climb past the target lines.",
    run: async () => {
      await admin("/admin/path", { mode: "direct", viewers: 1000 });
      return "1,000 dashboards are querying the main database directly.";
    },
  },
  {
    title: "Flip to Cuboid",
    say: "Same load, same database. Dashboard queries on the main database drop to zero and the app returns under the line.",
    run: async () => {
      await admin("/admin/path", { mode: "cuboid", viewers: 1000 });
      return "1,000 dashboards now read from Cuboid.";
    },
  },
  {
    title: "The PRD, live",
    say: "The PRD's four examples on the largest store, each with its freshness.",
    run: async (keys) => {
      const k = storeKey(keys, STORE);
      const sum = await get<Envelope<{ orders: number; sales: string }>>(`/tenants/${STORE}/orders/summary?range=last_7d`, k);
      const trend = await get<Envelope<{ points: unknown[] }>>(`/tenants/${STORE}/orders/trend?interval=day&range=last_30d`, k);
      const top = await get<Envelope<{ items: { label: string }[] }>>(`/tenants/${STORE}/products/top?n=10&by=sales&range=this_week`, k);
      const st = await get<Envelope<{ items: { label: string; value: number }[] }>>(`/tenants/${STORE}/orders/breakdown?by=status&range=last_7d`, k);
      return `${count(sum.data.orders)} orders, ${rupees(sum.data.sales)} in the last 7 days; ${trend.data.points.length} days of trend; top product ${top.data.items[0]?.label ?? "none"}; ${st.data.items.map((i) => `${i.label} ${count(i.value)}`).join(", ")}. ${sum.freshness.updated}.`;
    },
  },
  {
    title: "New data shows up",
    say: "Add an order and the count goes up. Cancel it and it moves from placed to cancelled.",
    run: async (keys) => {
      const k = storeKey(keys, STORE);
      const from = "2001-01-01T00:00:00";
      const to = "2001-01-02T00:00:00";
      const q = `/tenants/${STORE}/orders/breakdown?by=status&from=${from}&to=${to}`;
      const start = performance.now();
      const o = await admin<{ order_id: number }>(`/admin/app/tenants/${STORE}/orders`, { created_at: "2001-01-01T12:00:00+05:30" });
      const until = async (want: string) => {
        for (let i = 0; i < 200; i++) {
          const r = await get<Envelope<{ items: { key: string; value: number }[] }>>(q, k);
          if (r.data.items.some((x) => x.key === want && x.value > 0)) return;
          await new Promise((res) => setTimeout(res, 100));
        }
        throw new Error(`The order did not appear as ${want} within 20 s`);
      };
      await until("placed");
      const inserted = performance.now() - start;
      const s2 = performance.now();
      await admin(`/admin/app/tenants/${STORE}/orders/${o.order_id}/cancel`, {});
      await until("cancelled");
      const cancelled = performance.now() - s2;
      await admin(`/admin/app/tenants/${STORE}/orders/${o.order_id}/delete`, {});
      return `Order ${o.order_id} visible in ${ms(inserted)}; placed to cancelled visible in ${ms(cancelled)}.`;
    },
  },
  {
    title: "Correct",
    say: "A check against the main database on arbitrary timestamps, while writes keep running.",
    run: async () => {
      const now = new Date();
      const to = new Date(now.getTime() - 3 * 3600 * 1000 - 17000);
      const from = new Date(to.getTime() - 9 * 86400 * 1000 - 4242000);
      const r = await post<Envelope<VerifyResult>>(`/tenants/${STORE}/verify`, operatorKey(), {
        endpoint: "orders/breakdown",
        params: { by: "status", from: from.toISOString(), to: to.toISOString() },
      });
      await admin("/admin/verify-batch?n=100");
      return `${r.data.verdict}: fence ${r.data.fence_lsn}, Cuboid ${ms(r.data.cuboid_ms)}, direct ${ms(r.data.direct_ms)}. A batch of 100 more checks is running.`;
    },
  },
  {
    title: "Separate",
    say: "INC-02. One store's key on another store is not found. Then the drills inject that exact class of bug.",
    run: async (keys) => {
      let notFound = false;
      try {
        await get(`/tenants/${OTHER}/orders/count?range=last_7d`, storeKey(keys, STORE));
      } catch (e) {
        notFound = e instanceof ApiError && e.problem.status === 404;
      }
      if (!notFound) throw new Error("Store 42's key was not refused on store 43");
      let faults = 0;
      let blocked = 0;
      let leaked = 0;
      for (const mode of ["A", "B", "C"]) {
        const r = await admin<{ faults_injected: number; blocked: number; leaked: number }>("/admin/drill", { mode, seconds: 4 });
        faults += r.faults_injected;
        blocked += r.blocked;
        leaked += r.leaked;
      }
      if (leaked > 0) throw new Error(`The drill leaked ${leaked} answers`);
      return `Store 42's key on store 43: 404. Drills A, B and C: ${count(faults)} faults, ${count(blocked)} blocked, 0 leaked.`;
    },
  },
  {
    title: "The bonuses",
    say: "The biggest store floods heavy queries and nobody else notices. A new breakdown from three lines of YAML, built from the mirror.",
    run: async () => {
      const f = await admin<{ heavy_limited: number; light_limited: number; others_p99_before_ms: number; others_p99_during_ms: number }>("/admin/flood", { store: STORE, seconds: 5 });
      await admin("/admin/demo/metric", { add: false });
      // The build scans every order, minutes at 10M. It runs online, so the demo
      // moves on and the last beat shows it finished, with queries answered throughout.
      metricBuild = admin<{ ok: boolean; took_ms: number; error?: string }>("/admin/demo/metric", { add: true });
      return `${count(f.heavy_limited + f.light_limited)} queries from the flooding store limited; other stores' p99 ${ms(f.others_p99_before_ms)} before, ${ms(f.others_p99_during_ms)} during. Three lines of YAML added: breakdown by payment method is building from the mirror, without touching the main database, while queries keep answering.`;
    },
  },
  {
    title: "The scorecard",
    say: "The PRD's targets with measured values. You are looking at the monitoring page: bonus B-5.",
    run: async () => {
      window.scrollTo({ top: 0 });
      return "Every value comes from the latest make targets artifact.";
    },
  },
  {
    title: "Close",
    say: "Fast for every store. Exact to the paisa. Never in the main database's way, and never in the wrong tenant's hands.",
    run: async (keys) => {
      if (!metricBuild) return "End of the demo.";
      const add = await metricBuild;
      metricBuild = null;
      if (!add.ok) return `The new metric was not applied: ${add.error ?? "unknown error"}.`;
      const b = await get<Envelope<{ items: { label: string }[] }>>(`/tenants/${STORE}/orders/breakdown?by=payment_method&range=last_7d`, storeKey(keys, STORE));
      return `Breakdown by payment method is live, built in ${ms(add.took_ms)}: ${b.data.items.map((i) => i.label).join(", ")}. End of the demo.`;
    },
  },
];

export function Autopilot({ onClose }: { onClose: () => void }) {
  const [i, setI] = useState(-1);
  const [status, setStatus] = useState("Press Next or the space bar to start.");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const started = useRef(0);

  const next = useCallback(async () => {
    if (busy) return;
    const n = i + 1;
    if (n >= beats.length) {
      onClose();
      return;
    }
    if (n === 0) started.current = Date.now();
    setI(n);
    setBusy(true);
    setFailed(false);
    setStatus("Running…");
    try {
      const keys = await loadKeys();
      setStatus(await beats[n].run(keys));
    } catch (e) {
      setFailed(true);
      setStatus(e instanceof Error ? e.message : "This step failed.");
    } finally {
      setBusy(false);
    }
  }, [busy, i, onClose]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.code === "Space" && (e.target as HTMLElement).tagName !== "BUTTON") {
        e.preventDefault();
        next();
      }
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [next, onClose]);

  const beat = beats[i];
  return (
    <aside className="autopilot stack" aria-label="Demo" data-testid="autopilot" data-step={i} data-busy={busy} data-failed={failed}>
      <div className="row">
        <strong>{beat ? `${i + 1} of ${beats.length}: ${beat.title}` : "Demo"}</strong>
        <span className="push row">
          <button className="primary" onClick={next} disabled={busy}>
            {i + 1 >= beats.length ? "Finish" : "Next"}
          </button>
          <button onClick={onClose}>Stop</button>
        </span>
      </div>
      {beat && <p>{beat.say}</p>}
      <p className={failed ? "alarm" : "label"} role="status">
        {status}
      </p>
    </aside>
  );
}
