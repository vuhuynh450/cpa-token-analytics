// Pure helpers for the USD→VND exchange-rate feature. Kept free of React
// and i18n so they can run under `node --test`.

// Effective USD→VND rate: the positive finite number that converts USD
// amounts for display. Returns 0 when the rate is unavailable — callers must
// treat 0 as "no rate" and never render a made-up number. In manual mode a
// saved manual_rate is the fallback when the effective rate is missing.
export function fxEffectiveRate(fx) {
  if (!fx) return 0;
  const rate =
    typeof fx.rate === "number" && Number.isFinite(fx.rate) && fx.rate > 0
      ? fx.rate
      : null;
  if (rate !== null) return rate;
  if (fx.mode === "manual") {
    const m = fx.manual_rate;
    if (typeof m === "number" && Number.isFinite(m) && m > 0) return m;
  }
  return 0;
}

// Convert a USD amount to VND with the effective rate. Returns null when the
// input or the rate is unavailable — never 0 for "unknown".
export function usdToVnd(usd, rate) {
  if (
    typeof usd !== "number" ||
    !Number.isFinite(usd) ||
    typeof rate !== "number" ||
    !Number.isFinite(rate) ||
    rate <= 0
  ) {
    return null;
  }
  return usd * rate;
}

// Parse the manual-rate input. Two outcomes: a finite number > 0, or null
// (reject). Unlike parsePrice, empty is rejected (manual mode requires a
// rate) and only plain decimal text is accepted — "0x10", "0b101" and
// scientific junk like "1e" never become a rate.
export function parseManualRate(v) {
  if (v == null) return null;
  const s = String(v).trim();
  if (s === "") return null;
  if (!/^[+-]?(\d+(\.\d+)?|\.\d+)([eE][+-]?\d+)?$/.test(s)) return null;
  const n = Number(s);
  return Number.isFinite(n) && n > 0 ? n : null;
}

// Display name for the rate source: external providers keep their own name,
// "manual" maps to a sentinel the UI translates.
export function fxSource(fx) {
  const s = fx?.source;
  return typeof s === "string" && s ? s : null;
}

// Timestamp shown as "Last updated": the provider's own timestamp wins, the
// last fetch time is the fallback. Returns a Date or null.
export function fxUpdatedAt(fx) {
  const raw = fx?.provider_updated_at ?? fx?.fetched_at;
  if (!raw) return null;
  const d = new Date(raw);
  return Number.isNaN(d.getTime()) ? null : d;
}

// Age of a timestamp in minutes, rounded up so a fresh sync reads "0 min".
// Invalid/missing input and future timestamps yield null.
export function rateAgeMinutes(ts, now = Date.now()) {
  if (typeof ts !== "string" && typeof ts !== "number" && !(ts instanceof Date)) {
    return null;
  }
  const t = ts instanceof Date ? ts.getTime() : new Date(ts).getTime();
  if (!Number.isFinite(t) || t > now) return null;
  return Math.max(0, Math.ceil((now - t) / 60000));
}

// Card-level status for the FX card:
//   "syncing"     — a fetch is in flight right now
//   "unavailable" — no usable rate at all
//   "failed"      — the last sync attempt errored (rate may still be shown)
//   "cached"      — serving a stale rate (past its freshness window)
//   "fresh"       — up to date (auto source, or a valid manual entry)
export function fxState(fx) {
  if (fx?.syncing) return "syncing";
  if (!fxEffectiveRate(fx)) return "unavailable";
  if (fx?.last_error) return "failed";
  if (fx?.stale) return "cached";
  return "fresh";
}
