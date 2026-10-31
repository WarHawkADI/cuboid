// Indian number formatting. Dashboards abbreviate money (₹4.2 Cr, ₹18.4 L);
// the verification ledger shows it to the paisa (₹4,21,93,118.40).

export function groupIndian(intPart: string): string {
  const neg = intPart.startsWith("-");
  const s = neg ? intPart.slice(1) : intPart;
  if (s.length <= 3) return (neg ? "-" : "") + s;
  const tail = s.slice(-3);
  let head = s.slice(0, -3);
  const parts: string[] = [];
  while (head.length > 2) {
    parts.unshift(head.slice(-2));
    head = head.slice(0, -2);
  }
  if (head) parts.unshift(head);
  return (neg ? "-" : "") + parts.join(",") + "," + tail;
}

export function count(v: string | number): string {
  return groupIndian(String(v));
}

export function rupeesFull(v: string | number): string {
  const [i, f] = String(v).split(".");
  return `₹${groupIndian(i)}${f ? "." + f : ""}`;
}

export function rupees(v: string | number): string {
  const n = Number(v);
  const abs = Math.abs(n);
  if (abs >= 1e7) return `₹${(n / 1e7).toFixed(abs >= 1e9 ? 0 : 1)} Cr`;
  if (abs >= 1e5) return `₹${(n / 1e5).toFixed(1)} L`;
  return `₹${groupIndian(String(Math.round(n)))}`;
}

export function ms(v: number): string {
  if (v >= 1000) return `${(v / 1000).toFixed(1)} s`;
  if (v >= 10) return `${v.toFixed(0)} ms`;
  return `${v.toFixed(1)} ms`;
}

export function bytes(v: number): string {
  if (v >= 1 << 30) return `${(v / (1 << 30)).toFixed(1)} GB`;
  if (v >= 1 << 20) return `${(v / (1 << 20)).toFixed(0)} MB`;
  return `${(v / 1024).toFixed(0)} KB`;
}

export function clock(t: number | string): string {
  return new Date(t).toLocaleTimeString("en-IN", { hour12: false });
}

export const RANGES: { value: string; label: string; phrase: string }[] = [
  { value: "today", label: "Today", phrase: "today" },
  { value: "yesterday", label: "Yesterday", phrase: "yesterday" },
  { value: "last_7d", label: "Last 7 days", phrase: "in the last 7 days" },
  { value: "this_week", label: "This week", phrase: "this week" },
  { value: "this_month", label: "This month", phrase: "this month" },
  { value: "last_30d", label: "Last 30 days", phrase: "in the last 30 days" },
  { value: "last_90d", label: "Last 90 days", phrase: "in the last 90 days" },
  { value: "this_year", label: "This year", phrase: "this year" },
  { value: "all", label: "All time", phrase: "in all time" },
];
