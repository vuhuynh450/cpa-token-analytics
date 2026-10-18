import test from "node:test";
import assert from "node:assert/strict";
import {
  fxEffectiveRate,
  fxSource,
  fxState,
  fxUpdatedAt,
  parseManualRate,
  rateAgeMinutes,
  usdToVnd,
} from "./fx.js";

test("fxEffectiveRate returns a positive finite rate or 0", () => {
  assert.equal(fxEffectiveRate({ rate: 25000 }), 25000);
  assert.equal(fxEffectiveRate({ rate: 25500.5 }), 25500.5);
  // unavailable → 0, never a fabricated number
  assert.equal(fxEffectiveRate(null), 0);
  assert.equal(fxEffectiveRate(undefined), 0);
  assert.equal(fxEffectiveRate({}), 0);
  assert.equal(fxEffectiveRate({ rate: null }), 0);
  assert.equal(fxEffectiveRate({ rate: 0 }), 0);
  assert.equal(fxEffectiveRate({ rate: -5 }), 0);
  assert.equal(fxEffectiveRate({ rate: NaN }), 0);
  assert.equal(fxEffectiveRate({ rate: Infinity }), 0);
  assert.equal(fxEffectiveRate({ rate: "25000" }), 0);
  // manual fallback: saved manual_rate covers a missing effective rate
  assert.equal(
    fxEffectiveRate({ mode: "manual", rate: null, manual_rate: 24000 }),
    24000,
  );
  assert.equal(
    fxEffectiveRate({ mode: "auto", rate: null, manual_rate: 24000 }),
    0,
  );
  assert.equal(
    fxEffectiveRate({ mode: "manual", rate: null, manual_rate: null }),
    0,
  );
});

test("usdToVnd converts with the effective rate and guards junk", () => {
  assert.equal(usdToVnd(2, 25000), 50000);
  assert.equal(usdToVnd(0, 25000), 0);
  assert.equal(usdToVnd(1.5, 25100), 37650);
  // no rate → null (unavailable), not 0
  assert.equal(usdToVnd(10, 0), null);
  assert.equal(usdToVnd(10, null), null);
  assert.equal(usdToVnd(10, NaN), null);
  assert.equal(usdToVnd(10, -1), null);
  assert.equal(usdToVnd(null, 25000), null);
  assert.equal(usdToVnd("5", 25000), null);
});

test("parseManualRate only accepts plain positive decimals", () => {
  assert.equal(parseManualRate("25000"), 25000);
  assert.equal(parseManualRate(" 25500.5 "), 25500.5);
  assert.equal(parseManualRate(".5"), 0.5);
  assert.equal(parseManualRate(24000), 24000);
  // empty / zero / negative rejected
  assert.equal(parseManualRate(""), null);
  assert.equal(parseManualRate("   "), null);
  assert.equal(parseManualRate(null), null);
  assert.equal(parseManualRate(undefined), null);
  assert.equal(parseManualRate("0"), null);
  assert.equal(parseManualRate("-25000"), null);
  // non-decimal / non-finite text rejected
  assert.equal(parseManualRate("0x10"), null);
  assert.equal(parseManualRate("0b101"), null);
  assert.equal(parseManualRate("abc"), null);
  assert.equal(parseManualRate("25,000"), null);
  assert.equal(parseManualRate("1e"), null);
  assert.equal(parseManualRate("Infinity"), null);
  assert.equal(parseManualRate(NaN), null);
  assert.equal(parseManualRate(Infinity), null);
});

test("fxSource keeps provider names and flags manual", () => {
  assert.equal(fxSource({ source: "ExchangeRate-API" }), "ExchangeRate-API");
  assert.equal(fxSource({ source: "Fawaz Currency API" }), "Fawaz Currency API");
  assert.equal(fxSource({ source: "manual" }), "manual");
  assert.equal(fxSource({ source: null }), null);
  assert.equal(fxSource(null), null);
});

test("fxUpdatedAt prefers provider_updated_at then fetched_at", () => {
  const a = fxUpdatedAt({
    provider_updated_at: "2026-10-17T00:00:00Z",
    fetched_at: "2026-10-18T00:00:00Z",
  });
  assert.equal(a.toISOString(), "2026-10-17T00:00:00.000Z");
  const b = fxUpdatedAt({ provider_updated_at: null, fetched_at: "2026-10-18T00:00:00Z" });
  assert.equal(b.toISOString(), "2026-10-18T00:00:00.000Z");
  assert.equal(fxUpdatedAt({ provider_updated_at: null, fetched_at: null }), null);
  assert.equal(fxUpdatedAt({ provider_updated_at: "junk" }), null);
  assert.equal(fxUpdatedAt(null), null);
});

test("rateAgeMinutes reports whole minutes and guards junk", () => {
  const now = new Date("2026-10-18T12:00:00Z").getTime();
  assert.equal(rateAgeMinutes("2026-10-18T11:30:00Z", now), 30);
  assert.equal(rateAgeMinutes("2026-10-18T12:00:00Z", now), 0);
  assert.equal(rateAgeMinutes("2026-10-17T12:00:00Z", now), 1440);
  // invalid, missing and future timestamps → null
  assert.equal(rateAgeMinutes("junk", now), null);
  assert.equal(rateAgeMinutes(null, now), null);
  assert.equal(rateAgeMinutes("2026-10-18T12:30:00Z", now), null);
});

test("fxState prioritizes syncing, unavailable, failed, cached, fresh", () => {
  assert.equal(fxState({ syncing: true }), "syncing");
  assert.equal(fxState({ rate: null }), "unavailable");
  assert.equal(fxState(null), "unavailable");
  assert.equal(
    fxState({ rate: 25000, last_error: "timeout", stale: false }),
    "failed",
  );
  assert.equal(fxState({ rate: 25000, stale: true }), "cached");
  assert.equal(fxState({ rate: 25000, stale: false, last_error: null }), "fresh");
  // syncing wins over everything else
  assert.equal(
    fxState({ syncing: true, rate: 25000, stale: true, last_error: "x" }),
    "syncing",
  );
  // manual fallback rate keeps the card out of "unavailable"
  assert.equal(
    fxState({ mode: "manual", rate: null, manual_rate: 24000 }),
    "fresh",
  );
});
