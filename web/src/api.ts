// The web UI uses only the public API and the operator API.

export interface DemoKey {
  kind: "operator" | "company" | "store";
  id: number;
  name: string;
  key: string;
  stores?: number[];
}

export interface Freshness {
  as_of?: string;
  lsn: string;
  lag_ms: number;
  updated: string;
  stale: boolean;
}

export interface Segment {
  grain: string;
  from: string;
  to: string;
  buckets: number;
}

export interface PlanInfo {
  rollup?: string;
  segments: Segment[];
  edges: { from: string; to: string }[];
  cells_estimate: number;
  edge_rows_estimate: number;
  // Set when a top-N list was found by walking month totals (ADR-011).
  top_bound?: {
    rollup: string;
    months_from_cells: number;
    slices_from_mirror: number;
    depth: number;
    keys_read: number;
  };
}

export interface Envelope<T> {
  tenant_id?: number;
  company_id?: number;
  stores?: number[];
  query: Record<string, unknown>;
  from: string;
  to: string;
  data: T;
  freshness: Freshness;
  meta: {
    plan: PlanInfo;
    cache: string;
    exact: boolean;
    heavy: boolean;
    timing_ms: { total: number; store: number };
  };
}

export interface Item {
  rank?: number;
  tenant_id?: number;
  key: string | number | null;
  label: string;
  value: string | number;
  share: number;
}

export interface Problem {
  status: number;
  type: string;
  title: string;
  detail?: string;
  hint?: string;
}

export class ApiError extends Error {
  constructor(public problem: Problem) {
    super(problem.detail ?? problem.title);
  }
}

export interface VerifyResult {
  exact: boolean;
  verdict: string;
  mode: string;
  fence_lsn: string;
  r_minus: number;
  r_plus: number;
  retries: number;
  cuboid_ms: number;
  direct_ms: number;
  direct_sql: string;
  measures: string[];
  cuboid: { key: string[]; values: string[] }[];
  direct: { key: string[]; values: string[] }[];
  rows_compared: number;
  mismatches?: string[];
}

let keys: DemoKey[] = [];

export async function loadKeys(): Promise<DemoKey[]> {
  if (keys.length) return keys;
  const r = await fetch("/api/demo/keys");
  if (!r.ok) return [];
  const body = (await r.json()) as { keys: DemoKey[] };
  keys = body.keys;
  return keys;
}

export function operatorKey(): string {
  return keys.find((k) => k.kind === "operator")?.key ?? "";
}

async function parse<T>(r: Response): Promise<T> {
  const text = await r.text();
  let body: unknown = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    throw new ApiError({ status: r.status, type: "bad_response", title: "Unreadable response", detail: text.slice(0, 200) });
  }
  if (!r.ok) throw new ApiError({ type: "error", title: `The request failed (${r.status})`, ...(body as Partial<Problem>), status: r.status });
  return body as T;
}

export async function get<T>(path: string, key: string): Promise<T> {
  return parse<T>(await fetch(path, { headers: { Authorization: `Bearer ${key}` } }));
}

export async function post<T>(path: string, key: string, body?: unknown): Promise<T> {
  return parse<T>(
    await fetch(path, {
      method: "POST",
      headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
  );
}

export async function admin<T>(path: string, body?: unknown): Promise<T> {
  await loadKeys();
  return post<T>(path, operatorKey(), body);
}

export async function publicJSON<T>(path: string): Promise<T> {
  return parse<T>(await fetch(path));
}
