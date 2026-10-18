export function createFormatters(locale = "en-US") {
  const intlLocale = String(locale).startsWith("vi") ? "vi-VN" : "en-US";
  const fmt = (n) => Number(n || 0).toLocaleString(intlLocale);
  const compact = (n) =>
    new Intl.NumberFormat(intlLocale, {
      notation: "compact",
      maximumFractionDigits: 2,
    }).format(n || 0);
  return { fmt, compact };
}
export const { fmt, compact } = createFormatters();
export const dateKey = (d) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
export function summarize(events) {
  return events.reduce(
    (s, e) => ({
      n: s.n + 1,
      input: s.input + e.input,
      output: s.output + e.output,
      cache: s.cache + e.cache,
      reasoning: s.reasoning + e.reasoning,
      fail: s.fail + Number(e.failed),
    }),
    { n: 0, input: 0, output: 0, cache: 0, reasoning: 0, fail: 0 },
  );
}
export function groups(events, key) {
  const m = new Map();
  for (const e of events) {
    if (!m.has(e[key])) m.set(e[key], []);
    m.get(e[key]).push(e);
  }
  return [...m]
    .map(([name, evs]) => ({ name, ...summarize(evs) }))
    .sort((a, b) => b.input + b.output - (a.input + a.output));
}
export function trend(events, bucket) {
  const map = new Map();
  for (const e of events) {
    const d = new Date(e.ts * 1000),
      label =
        bucket === "month"
          ? dateKey(d).slice(0, 7)
          : bucket === "hour"
            ? `${dateKey(d)} ${String(d.getHours()).padStart(2, "0")}:00`
            : dateKey(d);
    if (!map.has(label))
      map.set(label, { label, cache: 0, net: 0, output: 0, requests: 0 });
    const b = map.get(label);
    b.cache += e.cache;
    b.net += Math.max(0, e.input - e.cache);
    b.output += e.output;
    b.requests++;
  }
  return [...map.values()].sort((a, b) => a.label.localeCompare(b.label));
}

// Parse a price input in the pricing override form. Three outcomes:
//   undefined — empty/blank/missing value; the field is omitted from the
//               payload (backend keeps the catalog price for that component)
//   number    — a finite value >= 0 (0 is a valid "free" price)
//   null      — invalid input: negative, NaN, Infinity or non-numeric text.
// "1e3" parses to 1000, matching <input type="number"> semantics.
export function parsePrice(v) {
  if (v == null) return undefined;
  const s = String(v).trim();
  if (s === "") return undefined;
  const n = Number(s);
  return Number.isFinite(n) && n >= 0 ? n : null;
}

// Format a ratio (0–1, the units contract for success_rate/cache_hit_rate/
// failure_rate) as a percent string. Non-finite input renders "—".
export function pctOf(v, digits = 1) {
  return typeof v === "number" && Number.isFinite(v)
    ? `${(v * 100).toFixed(digits)}%`
    : "—";
}

// Numeric validity gate (contract B2): only null/undefined/NaN/±Infinity are
// "unavailable" and render "—"; 0 is a VALID measurement (0%, 0 tokens,
// $0.00). `zeroIsMissing` opts out of that rule for columns where the API
// never emits 0 and it can only mean "no sample" (latency/ttft/tok_per_sec —
// summarizeDist drops non-positive values, so 0 cannot come back).
export function usable(v, zeroIsMissing = false) {
  return (
    typeof v === "number" && Number.isFinite(v) && !(zeroIsMissing && v === 0)
  );
}

// Format a count-like number; unavailable values render "—" instead of 0.
export function fmtNum(v, fmt = Math.round) {
  return usable(v) ? String(fmt(v)) : "—";
}

// Compact USD string — always two decimals so a real $0 reads "$0.00",
// never "$0"; unavailable renders "—". Locale-agnostic (plain digits).
export function usdStr(v, frac = 2) {
  return usable(v) ? `$${v.toFixed(frac)}` : "—";
}

// Coverage state for a priced/total event pair (contract B2 today/month/
// coverage blocks):
//   "none"    — no events at all (0/0) or missing data → caller shows "—"
//   "partial" — some events unpriced → caller adds a coverage indicator
//   "full"    — every event priced → no indicator needed
export function pricedCoverage(priced, total) {
  if (!usable(priced) || !usable(total) || total <= 0) {
    return { state: "none", priced: 0, total: 0 };
  }
  const p = Math.max(0, priced);
  return { state: p < total ? "partial" : "full", priced: p, total };
}

// Ordered heatmap cell list for the dow_hours payload shape
// { <metric>: [[7]x24], max: {<metric>: n} }. dow follows the contract
// (Mon=0 … Sun=6). Missing rows/values read as 0.
export function heatmapCells(dowHours, metric) {
  const cells = [];
  for (let dow = 0; dow < 7; dow++) {
    for (let hour = 0; hour < 24; hour++) {
      const v = dowHours?.[metric]?.[dow]?.[hour];
      cells.push({
        dow,
        hour,
        value: typeof v === "number" && Number.isFinite(v) ? v : 0,
      });
    }
  }
  return cells;
}

// Fill color for one heatmap cell: color-mix from chart-2 toward chart-3.
// ratio is value/max, clamped to [0,1]; 0 produces the muted base color.
export function heatColor(ratio) {
  const r =
    typeof ratio === "number" && Number.isFinite(ratio)
      ? Math.min(1, Math.max(0, ratio))
      : 0;
  if (r <= 0) return "var(--muted)";
  const pct = Math.min(100, 30 + Math.round(r * 70));
  return `color-mix(in srgb, var(--chart-3) ${pct}%, var(--chart-2))`;
}
