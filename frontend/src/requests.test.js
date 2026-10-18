import test from "node:test";
import assert from "node:assert/strict";
import {
  DEFAULT_FILTERS,
  DEFAULT_PAGE_SIZE,
  DEFAULT_SORT,
  advancedFilterCount,
  buildRequestParams,
  columnHasSort,
  cursorAt,
  hasActiveFilters,
  invalidMinKeys,
  nextSort,
  pageOf,
  parseMinValue,
  popCursor,
  pricingStatusKey,
  pricingStatusVariant,
  pushCursor,
  requestStatus,
  tokenParts,
  tokenSum,
  tsMillis,
} from "./requests.js";

test("buildRequestParams emits contract params and omits empties", () => {
  const p = buildRequestParams({
    ...DEFAULT_FILTERS,
    start: "2026-10-01",
    end: "2026-10-18",
    model: "gpt-5",
    status: "failed",
    slow: "s3",
    tokens: "t50k",
    minCost: "0.5",
    pricingStatus: "partial",
    search: "  key-a1 ",
    sort: "highest_latency",
    limit: 50,
  });
  assert.deepEqual(p, {
    start: "2026-10-01",
    end: "2026-10-18",
    model: "gpt-5",
    status: "failed",
    min_latency_ms: 3000,
    min_total_tokens: 50000,
    min_cost_usd: 0.5,
    pricing_status: "partial",
    search: "key-a1",
    sort: "highest_latency",
    limit: 50,
  });
});

test("buildRequestParams defaults: only sort/limit are always sent", () => {
  assert.deepEqual(buildRequestParams(), {
    sort: DEFAULT_SORT,
    limit: DEFAULT_PAGE_SIZE,
  });
  // "all"-sentinels and blanks produce no params
  const p = buildRequestParams({
    model: "all",
    status: "all",
    pricingStatus: "all",
    key: "   ",
    search: "  ",
  });
  assert.equal(p.model, undefined);
  assert.equal(p.status, undefined);
  assert.equal(p.pricing_status, undefined);
  assert.equal(p.key, undefined);
  assert.equal(p.search, undefined);
  assert.equal(p.min_latency_ms, undefined);
});

test("buildRequestParams: custom presets parse free text, junk is dropped", () => {
  const p = buildRequestParams({
    slow: "custom",
    minLatency: "1500",
    tokens: "custom",
    minTokens: "abc",
    minCost: "-3",
  });
  assert.equal(p.min_latency_ms, 1500);
  assert.equal(p.min_total_tokens, undefined);
  assert.equal(p.min_cost_usd, undefined);
  // unknown sort/limit fall back to contract defaults
  assert.equal(
    buildRequestParams({ sort: "bogus", limit: 7 }).sort,
    "newest",
  );
  assert.equal(buildRequestParams({ sort: "bogus", limit: 7 }).limit, 20);
});

test("buildRequestParams only sends cursor when present", () => {
  assert.equal(buildRequestParams({}, undefined).cursor, undefined);
  assert.equal(buildRequestParams({}, "").cursor, undefined);
  assert.equal(buildRequestParams({}, "abc123").cursor, "abc123");
});

test("parseMinValue: empty→undefined, valid→number, junk→null", () => {
  assert.equal(parseMinValue(""), undefined);
  assert.equal(parseMinValue("   "), undefined);
  assert.equal(parseMinValue(null), undefined);
  assert.equal(parseMinValue(undefined), undefined);
  assert.equal(parseMinValue("0"), 0);
  assert.equal(parseMinValue("2.5"), 2.5);
  assert.equal(parseMinValue(42), 42);
  assert.equal(parseMinValue("-1"), null);
  assert.equal(parseMinValue("abc"), null);
  assert.equal(parseMinValue("Infinity"), null);
  assert.equal(parseMinValue(NaN), null);
});

test("invalidMinKeys flags only custom/invalid thresholds", () => {
  assert.equal(invalidMinKeys({}).size, 0);
  assert.deepEqual(
    [...invalidMinKeys({ slow: "custom", minLatency: "-5" })],
    ["minLatency"],
  );
  // preset selected → its (empty) custom text is not invalid
  assert.equal(invalidMinKeys({ slow: "s3", minLatency: "-5" }).size, 0);
  assert.deepEqual(
    [...invalidMinKeys({ tokens: "custom", minTokens: "x", minCost: "y" })].sort(),
    ["minCost", "minTokens"],
  );
  assert.equal(
    invalidMinKeys({ slow: "custom", minLatency: "1200" }).size,
    0,
  );
});

test("hasActiveFilters / advancedFilterCount detect departures", () => {
  assert.equal(hasActiveFilters(DEFAULT_FILTERS), false);
  assert.equal(advancedFilterCount(DEFAULT_FILTERS), 0);
  assert.equal(hasActiveFilters({ ...DEFAULT_FILTERS, search: "x" }), true);
  assert.equal(hasActiveFilters({ ...DEFAULT_FILTERS, status: "failed" }), true);
  // a sort change alone is not a filter change
  assert.equal(
    hasActiveFilters({ ...DEFAULT_FILTERS, sort: "oldest" }),
    false,
  );
  const f = {
    ...DEFAULT_FILTERS,
    slow: "s5",
    minCost: "1",
    pricingStatus: "unpriced",
  };
  assert.equal(hasActiveFilters(f), true);
  assert.equal(advancedFilterCount(f), 3);
  // an invalid minCost still counts as set
  assert.equal(
    advancedFilterCount({ ...DEFAULT_FILTERS, minCost: "abc" }),
    1,
  );
});

test("cursor stack push/pop/page math", () => {
  let s = [];
  assert.equal(pageOf(s), 1);
  assert.equal(cursorAt(s), undefined);
  s = pushCursor(s, "c2");
  assert.equal(pageOf(s), 2);
  assert.equal(cursorAt(s), "c2");
  s = pushCursor(s, "c3");
  assert.equal(pageOf(s), 3);
  assert.equal(cursorAt(s), "c3");
  s = popCursor(s);
  assert.equal(pageOf(s), 2);
  assert.equal(cursorAt(s), "c2");
  // empty next_cursor cannot advance; pop past the bottom is a no-op
  assert.deepEqual(pushCursor(s, null), s);
  assert.deepEqual(popCursor([]), []);
});

test("nextSort cycles per column and resets on derived sorts", () => {
  assert.equal(nextSort("newest", "time"), "oldest");
  assert.equal(nextSort("oldest", "time"), "newest");
  assert.equal(nextSort("newest", "total"), "most_tokens");
  assert.equal(nextSort("most_tokens", "total"), "least_tokens");
  assert.equal(nextSort("highest_latency", "latency"), "newest");
  assert.equal(nextSort("newest", "cost"), "highest_cost");
  // derived-only sort has no column → any click resets to newest
  assert.equal(nextSort("failed_first", "time"), "newest");
  // non-sortable column always yields the default
  assert.equal(nextSort("oldest", "model"), "newest");
  assert.equal(columnHasSort("oldest", "time"), true);
  assert.equal(columnHasSort("failed_first", "time"), false);
});

test("requestStatus maps failed flag, missing → unknown", () => {
  assert.equal(requestStatus({ failed: true }), "failed");
  assert.equal(requestStatus({ failed: false }), "success");
  assert.equal(requestStatus({ failed: null }), "unknown");
  assert.equal(requestStatus({}), "unknown");
  assert.equal(requestStatus(null), "unknown");
});

test("pricingStatus helpers keep the contract values and guard junk", () => {
  assert.equal(pricingStatusKey("partial"), "partial");
  assert.equal(pricingStatusKey("nonsense"), null);
  assert.equal(pricingStatusVariant("priced"), "secondary");
  assert.equal(pricingStatusVariant("unpriced"), "destructive");
  assert.equal(pricingStatusVariant("partial"), "outline");
  assert.equal(pricingStatusVariant(undefined), "outline");
});

test("tokenParts decompose without overlap; base_input falls back", () => {
  const p = tokenParts({
    input: 1000,
    cache_read: 300,
    cache_write: 200,
    output: 400,
    reasoning: 120,
    total: 1500,
    base_input: 500,
  });
  assert.equal(p.baseInput, 500);
  // derived base = input - cache_read - cache_write when backend omits it
  const q = tokenParts({ input: 1000, cache_read: 300, cache_write: 200 });
  assert.equal(q.baseInput, 500);
  // legacy `cache` field works as cache_read
  assert.equal(tokenParts({ input: 10, cache: 4 }).cacheRead, 4);
  assert.equal(tokenParts({ input: 10, cache: 4 }).baseInput, 6);
  // junk/negative/undefined never produce negative or NaN parts
  const z = tokenParts({ input: "x", output: -5, total: null });
  assert.equal(z.input, 0);
  assert.equal(z.output, 0);
  assert.equal(z.total, null);
  // base cannot go below zero even if cache > input
  assert.equal(tokenParts({ input: 10, cache_read: 99 }).baseInput, 0);
});

test("tokenSum sums non-overlapping components, reasoning excluded", () => {
  const s = tokenSum({
    input: 1000,
    base_input: 500,
    cache_read: 300,
    cache_write: 200,
    output: 400,
    reasoning: 150,
    total: 1400,
  });
  // 500 + 300 + 200 + 400 — reasoning stays out (inside output)
  assert.equal(s.accounted, 1400);
  assert.equal(s.reported, 1400);
  // missing tokens object is a safe no-op
  const e = tokenSum(undefined);
  assert.equal(e.accounted, 0);
  assert.equal(e.reported, null);
});

test("tsMillis normalizes seconds, millis and ISO strings", () => {
  assert.equal(tsMillis(1760000000), 1760000000000);
  assert.equal(tsMillis(1760000000000), 1760000000000);
  assert.equal(
    tsMillis("2026-10-18T12:00:00Z"),
    Date.parse("2026-10-18T12:00:00Z"),
  );
  assert.equal(tsMillis(null), null);
  assert.equal(tsMillis("junk"), null);
  assert.equal(tsMillis(undefined), null);
});
