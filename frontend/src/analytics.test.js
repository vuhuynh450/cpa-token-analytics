import test from "node:test";
import assert from "node:assert/strict";
import {
  createFormatters,
  parsePrice,
  summarize,
  groups,
  trend,
  dateKey,
  pctOf,
  usable,
  fmtNum,
  usdStr,
  pricedCoverage,
  heatmapCells,
  heatColor,
} from "./analytics.js";
const fixture = [
  {
    ts: new Date(2026, 0, 1, 12).getTime() / 1000,
    model: "a",
    key: "k",
    input: 100,
    cache: 60,
    output: 20,
    reasoning: 5,
    failed: false,
  },
  {
    ts: new Date(2026, 0, 1, 13).getTime() / 1000,
    model: "a",
    key: "k",
    input: 0,
    cache: 0,
    output: 0,
    reasoning: 0,
    failed: true,
  },
];
test("tokens are not double counted and zero-token failures are included", () => {
  const s = summarize(fixture);
  assert.equal(s.input + s.output, 120);
  assert.equal(s.n, 2);
  assert.equal(s.fail, 1);
});
test("grouping and trend use same total", () => {
  assert.equal(groups(fixture, "model")[0].n, 2);
  const b = trend(fixture, "day")[0];
  assert.equal(b.cache + b.net + b.output, 120);
});
test("local date does not use UTC slicing", () =>
  assert.equal(dateKey(new Date(2026, 0, 1)), "2026-01-01"));

test("createFormatters honors locale and precision", () => {
  const en = createFormatters("en-US");
  const vi = createFormatters("vi-VN");
  assert.equal(en.fmt(1234567), "1,234,567");
  assert.equal(vi.fmt(1234567), "1.234.567");
  // compact keeps at most 2 fraction digits and never throws
  assert.match(en.compact(1234567), /^1\.23M$/);
  assert.equal(en.compact(1500), "1.5K");
  assert.equal(en.compact(0), "0");
  assert.equal(en.compact(null), "0");
  const viCompact = vi.compact(1234567);
  assert.match(viCompact, /1[,.]23/);
  // locale prefix aliases also work
  assert.equal(createFormatters("vi").fmt(1234), "1.234");
  assert.equal(createFormatters("en").fmt(1234), "1,234");
});

test("parsePrice distinguishes empty, valid and invalid inputs", () => {
  // empty/blank/missing → undefined (field omitted from payload)
  assert.equal(parsePrice(""), undefined);
  assert.equal(parsePrice("   "), undefined);
  assert.equal(parsePrice(null), undefined);
  assert.equal(parsePrice(undefined), undefined);
  // zero and positives are valid prices
  assert.equal(parsePrice("0"), 0);
  assert.equal(parsePrice(0), 0);
  assert.equal(parsePrice("2.5"), 2.5);
  assert.equal(parsePrice(" 4.2 "), 4.2);
  // scientific notation parses, matching <input type="number">
  assert.equal(parsePrice("1e3"), 1000);
  // negative, non-finite and junk text → null (invalid)
  assert.equal(parsePrice("-1"), null);
  assert.equal(parsePrice("-0.5"), null);
  assert.equal(parsePrice("abc"), null);
  assert.equal(parsePrice("1,5"), null);
  assert.equal(parsePrice("Infinity"), null);
  assert.equal(parsePrice(NaN), null);
  assert.equal(parsePrice(Infinity), null);
});

test("pctOf formats ratios as percent and guards junk", () => {
  assert.equal(pctOf(0.666), "66.6%");
  assert.equal(pctOf(1), "100.0%");
  assert.equal(pctOf(0), "0.0%");
  assert.equal(pctOf(0.5, 0), "50%");
  assert.equal(pctOf(null), "—");
  assert.equal(pctOf(NaN), "—");
  assert.equal(pctOf("0.5"), "—");
});

test("usable keeps zero as a valid measurement", () => {
  assert.equal(usable(0), true);
  assert.equal(usable(0.5), true);
  assert.equal(usable(-1), true);
  // only non-numbers/non-finite are unavailable
  assert.equal(usable(null), false);
  assert.equal(usable(undefined), false);
  assert.equal(usable(NaN), false);
  assert.equal(usable(Infinity), false);
  assert.equal(usable(-Infinity), false);
  assert.equal(usable("0"), false);
  // zeroIsMissing opt-out for sample-based measurements
  assert.equal(usable(0, true), false);
  assert.equal(usable(0.5, true), true);
});

test("fmtNum renders 0 as data and unavailable as dash", () => {
  assert.equal(fmtNum(0), "0");
  assert.equal(fmtNum(12.7), "13");
  assert.equal(fmtNum(12.7, (v) => v.toFixed(1)), "12.7");
  assert.equal(fmtNum(null), "—");
  assert.equal(fmtNum(undefined), "—");
  assert.equal(fmtNum(NaN), "—");
  assert.equal(fmtNum(Infinity), "—");
});

test("usdStr renders $0.00 for real zero and dash for missing", () => {
  assert.equal(usdStr(0), "$0.00");
  assert.equal(usdStr(1.5), "$1.50");
  assert.equal(usdStr(0.004, 4), "$0.0040");
  assert.equal(usdStr(null), "—");
  assert.equal(usdStr(undefined), "—");
  assert.equal(usdStr(NaN), "—");
  assert.equal(usdStr(Infinity), "—");
});

test("pricedCoverage classifies none/partial/full", () => {
  assert.deepEqual(pricedCoverage(0, 0), { state: "none", priced: 0, total: 0 });
  assert.deepEqual(pricedCoverage(12, 30), {
    state: "partial",
    priced: 12,
    total: 30,
  });
  assert.deepEqual(pricedCoverage(30, 30), {
    state: "full",
    priced: 30,
    total: 30,
  });
  // missing/invalid data → none
  assert.equal(pricedCoverage(null, 10).state, "none");
  assert.equal(pricedCoverage(5, undefined).state, "none");
  assert.equal(pricedCoverage(5, 0).state, "none");
  assert.equal(pricedCoverage(5, -3).state, "none");
  // fully priced plus priced > total (defensive) stays full
  assert.equal(pricedCoverage(30, 20).state, "full");
});

test("heatmapCells maps 7x24 dow_hours with Mon=0 ordering", () => {
  const requests = Array.from({ length: 7 }, () => Array(24).fill(0));
  requests[0][3] = 9; // Monday 03:00
  requests[6][23] = 7; // Sunday 23:00
  const cells = heatmapCells({ requests, max: { requests: 9 } }, "requests");
  assert.equal(cells.length, 168);
  assert.deepEqual(cells[0], { dow: 0, hour: 0, value: 0 });
  assert.deepEqual(cells[3], { dow: 0, hour: 3, value: 9 });
  const last = cells[cells.length - 1];
  assert.deepEqual(last, { dow: 6, hour: 23, value: 7 });
  // missing/malformed data reads as 0
  assert.equal(heatmapCells({}, "tokens").length, 168);
  assert.equal(heatmapCells(null, "tokens")[0].value, 0);
});

test("heatColor clamps ratio and uses muted base at zero", () => {
  assert.equal(heatColor(0), "var(--muted)");
  assert.equal(heatColor(-1), "var(--muted)");
  assert.equal(heatColor(NaN), "var(--muted)");
  assert.match(heatColor(0.5), /^color-mix\(in srgb, var\(--chart-3\) \d+%, var\(--chart-2\)\)$/);
  assert.match(heatColor(2), /100%/);
});
