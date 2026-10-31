// Records the narrated demo on the live stack, each action timed to the voice
// track. Frames and their timestamps go to outDir; tools/make-video.py joins them
// with the audio. Usage: node e2e/record-demo.mjs outDir coverPng
import { chromium } from "playwright";
import fs from "node:fs";

const base = process.env.CUBOID_URL ?? "http://localhost:8080";
const out = process.argv[2] ?? "../data/video/frames";
const cover = fs.readFileSync(process.argv[3] ?? "../data/video/cover.png").toString("base64");
fs.rmSync(out, { recursive: true, force: true });
fs.mkdirSync(out, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const CAPTIONS = [
  [0, "We're team iiitdards, and this is Cuboid."],
  [3, "10 million orders, 100 stores and 1,000 dashboards, all on one laptop."],
  [8, "Right now these dashboards are querying the main database directly."],
  [11, "This line is the store app's own query time."],
  [14, "It climbed from about 1 millisecond to about 100."],
  [17, "That's the problem in the PRD's own words:"],
  [19, "one big store's report makes the app slow for all stores."],
  [23, "Same load and same database, we switched these dashboards to Cuboid."],
  [27, "Dashboard queries on the main database drop to zero,"],
  [30, "and the app's query time falls back to about a millisecond."],
  [33, "Cuboid reads the database's change stream and keeps exact rollups in a separate store."],
  [38, "Dashboards never touch the main database."],
  [41, "There are four main queries."],
  [43, "Orders, sales, a daily trend, top products and a breakdown by status."],
  [47, "Uncached, they answer in under 40 milliseconds,"],
  [49, "and every response states how fresh it is."],
  [53, "To add new data, we add an order and it shows up in about a second."],
  [57, "Cancel it and it moves from placed to cancelled."],
  [60, "But how do we know that the numbers are right?"],
  [63, "Click any one. This shows exactly how it was assembled:"],
  [66, "whole months, days and hours, plus two thin edges of raw rows."],
  [69, "So any timestamp is exact."],
  [71, "Now we check it against the main database, at the same point in the log,"],
  [74, "while writes keep running."],
  [75, "Exact match."],
  [77, "Across 2,500 checks, every one of these matched."],
  [80, "This same check once caught a bug that lost two orders,"],
  [82, "which unit tests could not see."],
  [86, "INC-02 was a cache bug that showed users other users' data."],
  [90, "So we injected that same kind of bug into our own code."],
  [93, "This drill hands requests the wrong rows,"],
  [95, "and the result check stops them."],
  [97, "A million randomized requests, and zero were leaked."],
  [100, "20 hard kills under writes,"],
  [103, "and we were back in about three seconds,"],
  [104, "and the queries were still exact."],
  [107, "In terms of bonuses, the biggest store floods heavy queries"],
  [110, "and gets limited, while other stores don't notice."],
  [112, "One line of YAML adds a new breakdown,"],
  [115, "with no restart and no load on the main database."],
  [118, "The same engine also serves SOC alerts through config alone."],
  [123, "Here's what we measured."],
  [125, "Freshness and correctness hit Great."],
  [126.5, "Speed and database safety hit Good."],
  [128, "And one query, top customers, missed the growth target."],
  [132, "We showed you that because every number we display"],
  [134, "comes from a file you can check."],
  [137, "To conclude, this is our product, Cuboid."],
  [140, "It is fast for every store."],
  [142, "It is exact to the paisa."],
  [143, "And it is never in the wrong store's hands."],
  [145, "Thank you."],
  [146.6, ""],
];

const body = await (await fetch(`${base}/api/demo/keys`)).json();
const op = body.keys.find((k) => k.kind === "operator").key;
const store42 = body.keys.find((k) => k.kind === "store" && k.id === 42).key;
const post = async (path, b) => {
  const r = await fetch(base + path, { method: "POST", headers: { Authorization: `Bearer ${op}`, "Content-Type": "application/json" }, body: b === undefined ? undefined : JSON.stringify(b) });
  return { status: r.status, body: await r.json().catch(() => ({})) };
};
const get = async (path, key) => (await fetch(base + path, { headers: { Authorization: `Bearer ${key}` } })).json();

// The new metric takes about a minute to build at 10M, so it is built first and
// the video says so.
await post("/admin/path", { mode: "off" });
await post("/admin/demo/metric", { add: false });
await sleep(15000);
const metric = await post("/admin/demo/metric", { add: true });
if (!metric.body.ok) throw new Error(`metric: ${JSON.stringify(metric.body)}`);
const builtIn = (metric.body.took_ms / 1000).toFixed(0);
console.log("metric built in", builtIn, "s");

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1440, height: 810 }, deviceScaleFactor: 4 / 3 });
const p = await ctx.newPage();
const errors = [];
p.on("pageerror", (e) => errors.push(e.message));
await p.goto(`${base}/monitor`);

await sleep(40000); // a real baseline with no dashboards
await post("/admin/path", { mode: "direct", viewers: 1000 });
await sleep(70000); // the recorder climbs before the voice starts

await p.evaluate(() => {
  const css = document.createElement("style");
  css.textContent = `
    html { scroll-behavior: smooth; }
    #v-cap { position: fixed; left: 50%; bottom: 14px; transform: translateX(-50%); max-width: 1180px; padding: 7px 18px; border-radius: 6px; background: rgba(17,25,34,.88); color: #fff; font: 600 21px/1.3 var(--ui, sans-serif); text-align: center; z-index: 2147483000; }
    #v-cap:empty { display: none; }
    #v-cursor { position: fixed; width: 24px; height: 24px; margin: -12px 0 0 -12px; border-radius: 50%; border: 3px solid #fff; background: rgba(40,70,196,.75); box-shadow: 0 0 0 2px rgba(17,25,34,.65); z-index: 2147483001; left: 720px; top: 440px; transition: left .55s ease, top .55s ease, transform .15s; pointer-events: none; }
    .v-card { position: fixed; right: 26px; bottom: 76px; width: 590px; background: #111922; color: #fff; border-radius: 10px; padding: 16px 20px; z-index: 2147482000; font: 400 18px/1.4 var(--ui, sans-serif); box-shadow: 0 10px 34px rgba(0,0,0,.4); }
    .v-card b { color: #A9B8FF; } .v-card code { font: 400 15px/1.45 var(--mono, monospace); color: #EEF1F4; display: block; white-space: pre-wrap; }
    .v-card .ok { color: #7FE0A8; font-weight: 700; } .v-card small { color: #BAC3CD; font-size: 15px; }
    #v-cover { position: fixed; inset: 0; width: 100vw; height: 100vh; z-index: 2147481000; opacity: 0; transition: opacity .7s; }
    .v-mark { outline: 3px solid var(--mark); outline-offset: -2px; }
  `;
  document.head.append(css);
  const cap = Object.assign(document.createElement("div"), { id: "v-cap" });
  const cur = Object.assign(document.createElement("div"), { id: "v-cursor" });
  document.documentElement.append(cap, cur);
});

const caption = (t) => p.evaluate((x) => { document.getElementById("v-cap").textContent = x; }, t);
const card = (id, html, style = "") =>
  p.evaluate(([i, h, s]) => {
    let c = document.getElementById(i);
    if (!c) { c = Object.assign(document.createElement("div"), { id: i, className: "v-card" }); document.documentElement.append(c); }
    c.innerHTML = h;
    c.style.cssText = s;
  }, [id, html, style]);
const dropCard = (id) => p.evaluate((i) => document.getElementById(i)?.remove(), id);
const press = async (loc) => {
  await loc.scrollIntoViewIfNeeded().catch(() => {});
  const b = await loc.boundingBox();
  if (b) {
    await p.evaluate(([x, y]) => { const c = document.getElementById("v-cursor"); c.style.left = x + "px"; c.style.top = y + "px"; }, [b.x + b.width / 2, b.y + b.height / 2]);
    await sleep(600);
    await p.evaluate(() => { document.getElementById("v-cursor").style.transform = "scale(.7)"; });
    await sleep(120);
    await p.evaluate(() => { document.getElementById("v-cursor").style.transform = ""; });
  }
  await loc.click();
};
const scrollToHeading = (text, block = "start") =>
  p.evaluate(([t, bl]) => {
    const h = [...document.querySelectorAll("h1,h2,h3")].find((x) => x.textContent.includes(t));
    h?.closest("section")?.scrollIntoView({ block: bl, behavior: "smooth" });
  }, [text, block]);
const mark = (rowText, colour) =>
  p.evaluate(([t, c]) => {
    const tr = [...document.querySelectorAll("tr")].find((x) => x.cells[0]?.textContent.trim() === t);
    if (tr) { tr.style.setProperty("--mark", c); tr.classList.add("v-mark"); }
  }, [rowText, colour]);

// ---- screencast, every frame with its own timestamp
const cdp = await ctx.newCDPSession(p);
const frames = [];
let n = 0;
cdp.on("Page.screencastFrame", async (f) => {
  const name = `f${String(n++).padStart(6, "0")}.jpg`;
  fs.writeFileSync(`${out}/${name}`, Buffer.from(f.data, "base64"));
  frames.push({ name, ts: f.metadata.timestamp });
  await cdp.send("Page.screencastFrameAck", { sessionId: f.sessionId }).catch(() => {});
});
await cdp.send("Page.startScreencast", { format: "jpeg", quality: 90, maxWidth: 1920, maxHeight: 1080, everyNthFrame: 1 });
await sleep(800);

const t0 = Date.now();
const until = (t) => sleep(Math.max(0, t0 + t * 1000 - Date.now()));

(async () => {
  for (const [t, text] of CAPTIONS) {
    await until(t);
    await caption(text);
  }
})();

const nav = (name) => p.getByRole("navigation", { name: "Views" }).getByRole("link", { name, exact: true });

// 1 and 2: the recorder, Direct then Cuboid
await until(24.2);
await press(p.getByRole("group", { name: "Dashboards' path" }).getByRole("button", { name: "Cuboid", exact: true }));

// 3: the four queries, on the largest store
await until(40.2);
await press(nav("Store"));
await sleep(1400);
await until(43.2);
await scrollToHeading("Orders per day");
await until(45);
await scrollToHeading("Top 10 products");
await until(49);
await p.evaluate(() => window.scrollTo({ top: 0, behavior: "smooth" }));

// 4: a new order, through the store app and back through the API
const from = "2001-01-01T00:00:00", to = "2001-01-02T00:00:00";
const breakdown = () => get(`/tenants/42/orders/breakdown?by=status&from=${from}&to=${to}`, store42);
const seen = async (status) => {
  for (let i = 0; i < 300; i++) {
    const r = await breakdown();
    if (r.data.items.some((x) => x.key === status && x.value > 0)) return;
    await sleep(40);
  }
  throw new Error(`order never showed as ${status}`);
};
const orderCard = (lines) => card("v-order", `<b>Live, through the API</b><br>${lines.join("<br>")}`);
await until(53.6);
let t1 = Date.now();
const made = await post("/admin/app/tenants/42/orders", { created_at: "2001-01-01T12:00:00+05:30" });
await orderCard([`Order ${made.body.order_id} added: <i>placed</i>`, "<small>waiting for it in the numbers…</small>"]);
await seen("placed");
const shown = ((Date.now() - t1) / 1000).toFixed(1);
await orderCard([`Order ${made.body.order_id} added: <i>placed</i>`, `<span class="ok">Visible in ${shown} s</span>`]);
await until(57.4);
t1 = Date.now();
await post(`/admin/app/tenants/42/orders/${made.body.order_id}/cancel`, {});
await seen("cancelled");
const moved = ((Date.now() - t1) / 1000).toFixed(1);
await orderCard([`Order ${made.body.order_id} added: <i>placed</i>`, `<span class="ok">Visible in ${shown} s</span>`, `Cancelled: <span class="ok">moved in ${moved} s</span>`]);
await post(`/admin/app/tenants/42/orders/${made.body.order_id}/delete`, {});
await until(60.2);
await dropCard("v-order");

// 5: open a number, then check it
await until(62.4);
await press(p.locator(".figure-v button").first());
await until(71);
await press(p.getByRole("button", { name: "Check against main database" }));
await p.locator(".drawer .exact").first().waitFor({ timeout: 12000 }).catch(() => console.log("no verdict in time"));
await p.locator(".drawer .exact").first().scrollIntoViewIfNeeded().catch(() => {});
await until(83.2);
await p.getByRole("dialog").getByRole("button", { name: "Close" }).click();

// 6: isolation, with drill B
await until(84.2);
await press(nav("Isolation"));
await until(89.4);
await press(p.getByRole("button", { name: "Run leak drill" }).nth(1));
await until(97.2);
await p.getByText("leaked", { exact: false }).first().scrollIntoViewIfNeeded().catch(() => {});
await card("v-stats", `<span class="ok">0 of 1,000,000</span> randomized requests crossed stores`);
await until(100);
await card("v-stats", `<span class="ok">0 of 1,000,000</span> randomized requests crossed stores<br><span class="ok">20 kills</span> under writes: back within 3.1 s, 500 of 500 checks exact`);
await until(106.6);
await dropCard("v-stats");

// 7: flood, the new metric, SOC alerts
await press(nav("Monitoring"));
await sleep(700);
await scrollToHeading("Controls", "end");
await press(p.getByRole("button", { name: "Flood heavy queries" }));
await until(112);
const pay = await get(`/tenants/42/orders/breakdown?by=payment_method&range=last_7d`, store42);
const list = pay.data.items.slice(0, 5).map((i) => `${i.label} ${Number(i.value).toLocaleString("en-IN")}`).join(" · ");
await card("v-yaml", `<b>config/metrics.yaml: one line added</b><code>status_payment: {source: orders, dims: [status, payment_method], grains: {hour: 35d, day: all, month: all}}</code><br>Breakdown by payment method, live:<br><span class="ok">${list}</span><br><small>The build took ${builtIn} s at 10M, shortened here.</small>`);
await until(117.6);
await dropCard("v-yaml");
// Another store: 42 has just been limited for flooding, which is the point of the flood.
await p.evaluate(() => { history.pushState({}, "", "/t/43"); dispatchEvent(new PopStateEvent("popstate")); });
await sleep(500);
await press(p.getByRole("button", { name: "SOC alerts" }));

// 8: what we measured
await until(122.8);
await nav("Monitoring").click();
await sleep(500);
await scrollToHeading("The PRD's targets");
await until(125);
await mark("Freshness", "#1D7A47"); await mark("Correctness", "#1D7A47");
await until(126.5);
await mark("Query speed", "#C2660E"); await mark("Database safety", "#C2660E");
await until(128);
await mark("Growth", "#C8222B");
await until(132);
await press(nav("Results"));
await sleep(800);
await scrollToHeading("Query time at 10 million");

// 9: close on the deck's cover
await until(136.8);
await p.evaluate((c) => {
  const i = Object.assign(document.createElement("img"), { id: "v-cover", src: "data:image/png;base64," + c });
  document.documentElement.append(i);
  document.getElementById("v-cursor").style.display = "none";
  requestAnimationFrame(() => requestAnimationFrame(() => { i.style.opacity = "1"; }));
}, cover);

await until(147.6);
const t0s = t0 / 1000;
await cdp.send("Page.stopScreencast");
fs.writeFileSync(`${out}/frames.json`, JSON.stringify({ t0: t0s, end: t0s + 147.6, frames }));
console.log("frames", frames.length, "browser errors", errors.length ? errors.join("; ") : "none");
await browser.close();

await post("/admin/path", { mode: "off" });
await post("/admin/demo/metric", { add: false });
console.log("done");
