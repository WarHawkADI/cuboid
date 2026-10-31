// Captures the recorder across real load: no dashboards, then 1,000 direct, then
// 1,000 through Cuboid, so one picture shows the bands and the drop.
// Usage: node e2e/recorder-shot.mjs [outDir]
import { chromium } from "playwright";

const base = process.env.CUBOID_URL ?? "http://localhost:8080";
const out = process.argv[2] ?? "../artifacts/screenshots";
const wait = (s) => new Promise((r) => setTimeout(r, s * 1000));
const keys = await (await fetch(`${base}/api/demo/keys`)).json();
const op = keys.keys.find((k) => k.kind === "operator").key;
const path = (mode) =>
  fetch(`${base}/admin/path`, { method: "POST", headers: { Authorization: `Bearer ${op}`, "Content-Type": "application/json" }, body: JSON.stringify({ mode, viewers: 1000 }) });

const browser = await chromium.launch();
const p = await browser.newPage({ viewport: { width: 1440, height: 760 }, deviceScaleFactor: 2 });
await p.goto(`${base}/monitor`);
await wait(45);
await path("direct");
await wait(60);
await path("cuboid");
await wait(75);
await p.screenshot({ path: `${out}/recorder-load.png` });
const state = await (await fetch(`${base}/api/monitor/state?since=9999999999999`)).json();
console.log("alarms", state.alarms.length, "lag_ms", state.freshness.lag_ms);
await path("off");
await browser.close();
