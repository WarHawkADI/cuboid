import "@fontsource-variable/atkinson-hyperlegible-next";
import "@fontsource/atkinson-hyperlegible-mono/400.css";
import "uplot/dist/uPlot.min.css";
import "./tokens.css";
import "./app.css";

import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Link, NavLink, Navigate, Route, Routes, useNavigate, useParams } from "react-router-dom";

import { DemoKey, loadKeys } from "./api";
import { Monitor } from "./views/Monitor";
import { Dashboard } from "./views/Dashboard";
import { Isolation } from "./views/Isolation";
import { Bench } from "./views/Bench";

function Mark() {
  return (
    <svg width="20" height="20" viewBox="0 0 16 16" fill="none" strokeWidth="1.4" aria-hidden="true">
      <path d="M2 5.5 7 3l5 2.5v6L7 14l-5-2.5z" stroke="var(--ink)" />
      <path d="M2 5.5 7 8l5-2.5M7 8v6" stroke="var(--ink)" />
      <path d="m9.5 1.5 4.5 2.2v6" stroke="var(--pen)" />
    </svg>
  );
}

function Rail({ keys }: { keys: DemoKey[] }) {
  const nav = useNavigate();
  return (
    <header className="rail">
      <Link to="/monitor" className="brand">
        <Mark /> Cuboid
      </Link>
      <nav className="nav" aria-label="Views">
        <NavLink to="/monitor">Monitoring</NavLink>
        <NavLink to="/t/42">Store</NavLink>
        <NavLink to="/isolation">Isolation</NavLink>
        <NavLink to="/bench">Results</NavLink>
      </nav>
      {keys.length > 0 && (
        <label className="push label">
          Demo key{" "}
          <select
            aria-label="Choose a demo key"
            value=""
            onChange={(e) => e.target.value && nav(e.target.value)}
          >
            <option value="">Open as…</option>
            {keys
              .filter((k) => k.kind !== "operator")
              .map((k) => (
                <option key={k.key} value={k.kind === "store" ? `/t/${k.id}` : `/c/${k.id}`}>
                  {k.kind === "store" ? `Store ${k.id}: ${k.name}` : `${k.name} (${k.stores?.length ?? 0} stores)`}
                </option>
              ))}
          </select>
        </label>
      )}
    </header>
  );
}

function StoreRoute({ keys, company }: { keys: DemoKey[]; company: boolean }) {
  const id = Number(useParams().id);
  return <Dashboard key={`${company}-${id}`} id={id} company={company} keys={keys} />;
}

function App() {
  const [keys, setKeys] = useState<DemoKey[]>([]);
  useEffect(() => {
    const tryLoad = () =>
      loadKeys().then((k) => {
        if (k.length > 1) setKeys(k);
        else setTimeout(tryLoad, 3000);
      });
    tryLoad();
  }, []);
  return (
    <BrowserRouter>
      <Rail keys={keys} />
      <Routes>
        <Route path="/" element={<Navigate to="/monitor" replace />} />
        <Route path="/monitor" element={<Monitor />} />
        <Route path="/t/:id" element={<StoreRoute keys={keys} company={false} />} />
        <Route path="/c/:id" element={<StoreRoute keys={keys} company />} />
        <Route path="/isolation" element={<Isolation />} />
        <Route path="/bench" element={<Bench />} />
      </Routes>
    </BrowserRouter>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
