// Pure helpers for the Requests inspector tab. Kept free of React and i18n
// so the module runs under `node --test` (see requests.test.js).

import { usable } from "./analytics.js";

// Server-side sort keys from the GET /api/requests contract.
export const SORT_KEYS = [
  "newest",
  "oldest",
  "most_tokens",
  "least_tokens",
  "highest_latency",
  "highest_cost",
  "failed_first",
];
export const DEFAULT_SORT = "newest";

export const PAGE_SIZES = [20, 50, 100];
export const DEFAULT_PAGE_SIZE = 20;

// Contract pricing_status values; "all" is a UI-only value (omits the param).
export const PRICING_STATUSES = ["priced", "partial", "unpriced", "ambiguous"];

// Preset thresholds for the "slow" and "high tokens" selects; "custom"
// defers to the free-text min_* inputs, "off" emits no param.
export const LATENCY_PRESETS = {
  off: undefined,
  s1: 1000,
  s3: 3000,
  s5: 5000,
  s10: 10000,
  custom: undefined,
};
export const TOKEN_PRESETS = {
  off: undefined,
  t10k: 10000,
  t50k: 50000,
  t100k: 100000,
  custom: undefined,
};

// Default (inactive) filter state; start/end come from the shared range
// picker and are injected when params are built.
export const DEFAULT_FILTERS = {
  model: "all",
  key: "",
  status: "all",
  sort: DEFAULT_SORT,
  slow: "off",
  minLatency: "",
  tokens: "off",
  minTokens: "",
  minCost: "",
  pricingStatus: "all",
  search: "",
};

// Parse a min_* threshold input. Three outcomes, same contract as
// analytics.parsePrice: undefined = empty (param omitted), null = invalid
// (rejected, never sent), number = a finite value >= 0.
export function parseMinValue(v) {
  if (v == null) return undefined;
  const s = String(v).trim();
  if (s === "") return undefined;
  const n = Number(s);
  return Number.isFinite(n) && n >= 0 ? n : null;
}

// Map filter/sort/pagination state to GET /api/requests query params.
// Empty/blank/"all"/invalid values are omitted; cursor is only sent on
// non-first pages.
export function buildRequestParams(f = {}, cursor) {
  const p = {};
  if (f.start) p.start = f.start;
  if (f.end) p.end = f.end;
  if (f.model && f.model !== "all") p.model = f.model;
  const key = String(f.key ?? "").trim();
  if (key) p.key = key;
  if (f.status && f.status !== "all") p.status = f.status;
  const lat =
    f.slow === "custom" ? parseMinValue(f.minLatency) : LATENCY_PRESETS[f.slow];
  if (usable(lat)) p.min_latency_ms = lat;
  const tok =
    f.tokens === "custom"
      ? parseMinValue(f.minTokens)
      : TOKEN_PRESETS[f.tokens];
  if (usable(tok)) p.min_total_tokens = tok;
  const cost = parseMinValue(f.minCost);
  if (usable(cost)) p.min_cost_usd = cost;
  if (f.pricingStatus && f.pricingStatus !== "all") {
    p.pricing_status = f.pricingStatus;
  }
  const search = String(f.search ?? "").trim();
  if (search) p.search = search;
  p.sort = SORT_KEYS.includes(f.sort) ? f.sort : DEFAULT_SORT;
  p.limit = PAGE_SIZES.includes(f.limit) ? f.limit : DEFAULT_PAGE_SIZE;
  if (cursor) p.cursor = cursor;
  return p;
}

// Inputs currently holding an invalid (non-empty, non-numeric/negative)
// threshold value — used to mark fields aria-invalid. Preset selections are
// never invalid; only "custom"/free-text values can be.
export function invalidMinKeys(f = {}) {
  const bad = new Set();
  if (f.slow === "custom" && parseMinValue(f.minLatency) === null) {
    bad.add("minLatency");
  }
  if (f.tokens === "custom" && parseMinValue(f.minTokens) === null) {
    bad.add("minTokens");
  }
  if (parseMinValue(f.minCost) === null) bad.add("minCost");
  return bad;
}

// Whether any filter departs from DEFAULT_FILTERS (drives the Clear button).
// An invalid threshold still counts as "set".
export function hasActiveFilters(f = {}) {
  return (
    (f.model && f.model !== "all") ||
    !!String(f.key ?? "").trim() ||
    (f.status && f.status !== "all") ||
    (f.slow && f.slow !== "off") ||
    (f.tokens && f.tokens !== "off") ||
    parseMinValue(f.minCost) !== undefined ||
    (f.pricingStatus && f.pricingStatus !== "all") ||
    !!String(f.search ?? "").trim()
  );
}

// Count of non-default advanced filters, for the badge on the popover
// trigger. Covers the filters living inside the advanced block only.
export function advancedFilterCount(f = {}) {
  let n = 0;
  if (f.slow && f.slow !== "off") n++;
  if (f.tokens && f.tokens !== "off") n++;
  if (parseMinValue(f.minCost) !== undefined) n++;
  if (f.pricingStatus && f.pricingStatus !== "all") n++;
  return n;
}

// Cursor pagination state is a stack: stack[i] is the cursor sent to fetch
// page i+2, so the current page is stack.length + 1 and the cursor for the
// current fetch is the stack top.
export function pushCursor(stack, next) {
  return next ? [...stack, next] : stack;
}
export function popCursor(stack) {
  return stack.slice(0, -1);
}
export function cursorAt(stack) {
  return stack.length ? stack[stack.length - 1] : undefined;
}
export function pageOf(stack) {
  return stack.length + 1;
}

// Column-header click → next sort. Non-toggleable columns fall back to the
// default sort; derived sorts (failed_first) are only reachable via the
// sort select, so they map to no column and reset to "newest" on click.
const SORT_CYCLE = {
  time: ["newest", "oldest"],
  total: ["most_tokens", "least_tokens"],
  latency: ["highest_latency", "newest"],
  cost: ["highest_cost", "newest"],
};
export function nextSort(current, column) {
  const cycle = SORT_CYCLE[column];
  if (!cycle) return DEFAULT_SORT;
  const i = cycle.indexOf(current);
  return cycle[i < 0 ? 0 : (i + 1) % cycle.length];
}
// Sorts grouped under each column header (for the active-arrow state).
export function columnHasSort(sort, column) {
  return (SORT_CYCLE[column] || []).includes(sort);
}
// Descending-family sorts render ArrowDown, ascending ArrowUp.
export const DESC_SORTS = new Set([
  "newest",
  "most_tokens",
  "highest_latency",
  "highest_cost",
  "failed_first",
]);

// Row status for the badge: the contract always sends `failed`, but a
// missing flag must render "unknown", not a guessed success.
export function requestStatus(row) {
  if (!row || row.failed === null || row.failed === undefined) {
    return "unknown";
  }
  return row.failed ? "failed" : "success";
}

// Badge variant for a pricing_status value. Unknown/absent values get a
// neutral outline and the caller falls back to a localized label.
export function pricingStatusKey(status) {
  return PRICING_STATUSES.includes(status) ? status : null;
}
export function pricingStatusVariant(status) {
  return (
    {
      priced: "secondary",
      partial: "outline",
      ambiguous: "outline",
      unpriced: "destructive",
    }[pricingStatusKey(status)] || "outline"
  );
}

const nonNeg = (v) => (usable(v) && v >= 0 ? v : 0);

// Decompose the detail `tokens` block for display WITHOUT overlapping sums:
// `input` contains cache reads (and, per the contract note, cache writes);
// `output` contains reasoning. base_input is the provider's own
// non-cache/non-write figure; when absent it is derived as the remainder.
export function tokenParts(t = {}) {
  const input = nonNeg(t.input);
  const cacheRead = nonNeg(t.cache_read ?? t.cache);
  const cacheWrite = nonNeg(t.cache_write);
  const baseInput = usable(t.base_input)
    ? Math.max(0, t.base_input)
    : Math.max(0, input - cacheRead - cacheWrite);
  return {
    input,
    baseInput,
    cacheRead,
    cacheWrite,
    output: nonNeg(t.output),
    reasoning: nonNeg(t.reasoning),
    total: usable(t.total) && t.total >= 0 ? t.total : null,
  };
}

// Sum of the non-overlapping components (base input + cache read +
// cache write + output) vs the reported total — reasoning stays out because
// it is already inside output.
export function tokenSum(t = {}) {
  const p = tokenParts(t);
  return {
    accounted: p.baseInput + p.cacheRead + p.cacheWrite + p.output,
    reported: p.total,
  };
}

// Normalize the `ts` field (unix seconds like /usage events, but accept
// millis and ISO strings defensively) to epoch millis. null on junk.
export function tsMillis(ts) {
  if (usable(ts)) return ts > 1e12 ? ts : ts * 1000;
  if (typeof ts === "string") {
    const d = Date.parse(ts);
    return Number.isNaN(d) ? null : d;
  }
  return null;
}
