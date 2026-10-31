// Renders the README's diagrams to PNGs for review. Usage: node e2e/svg-shots.mjs outDir
import { chromium } from "playwright";
import fs from "node:fs";
import path from "node:path";

const out = process.argv[2] ?? "../data/art";
const dir = "../docs/img";
fs.mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
for (const f of fs.readdirSync(dir).filter((x) => x.endsWith(".svg"))) {
  const p = await browser.newPage({ viewport: { width: 1320, height: 620 } });
  await p.goto("file:///" + path.resolve(dir, f).split(path.sep).join("/"));
  await p.screenshot({ path: `${out}/${f}.png` });
  await p.close();
}
await browser.close();
