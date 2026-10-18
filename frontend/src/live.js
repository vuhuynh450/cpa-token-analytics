// Pure helpers for the live-update polling loop. Kept free of React and
// browser APIs so the module runs under `node --test` (see live.test.js).

// Poll modes: live3/5/10 map to millisecond intervals, "paused" disables
// the timer entirely.
export const LIVE_INTERVALS = {
  live3: 3000,
  live5: 5000,
  live10: 10000,
  paused: 0,
};
export const LIVE_MODES = Object.keys(LIVE_INTERVALS);
export const DEFAULT_LIVE_MODE = "live3";
export const LIVE_STORAGE_KEY = "cpa-usage-live";

// Validate + normalize the persisted mode; anything unknown falls back to
// the default so a stale/corrupt localStorage value cannot wedge polling.
export function parseLiveMode(v) {
  return Object.hasOwn(LIVE_INTERVALS, v) ? v : DEFAULT_LIVE_MODE;
}
export function readLiveMode(getItem) {
  try {
    return parseLiveMode(getItem(LIVE_STORAGE_KEY));
  } catch {
    return DEFAULT_LIVE_MODE;
  }
}
export function writeLiveMode(mode, setItem) {
  try {
    setItem(LIVE_STORAGE_KEY, parseLiveMode(mode));
  } catch {
    /* storage unavailable — the mode simply won't persist */
  }
}

// Decode GET /api/changes. Contract:
//   {usage_revision:number, count:number, dataset_id:string, checked_at:iso}
// Returns {revision, datasetId} or null on any shape violation. `count`
// and `checked_at` are informational only.
export function decodeChanges(json) {
  if (!json || typeof json !== "object") return null;
  const rev = json.usage_revision;
  const ds = json.dataset_id;
  if (typeof rev !== "number" || !Number.isFinite(rev)) return null;
  if (typeof ds !== "string" || !ds) return null;
  return { revision: rev, datasetId: ds };
}

// Compare a poll result with the stored baseline.
//   "baseline" — first successful poll (or a malformed response); never
//                triggers a data refresh on its own
//   "refresh"  — (dataset_id, revision) changed → real new data
//   "none"     — unchanged tuple
// A dataset_id swap always means refresh even if the revision coincides.
export function decideAction(prev, parsed) {
  if (!parsed) return "baseline";
  if (!prev || prev.revision == null || !prev.datasetId) return "baseline";
  if (parsed.datasetId !== prev.datasetId) return "refresh";
  return parsed.revision === prev.revision ? "none" : "refresh";
}

// Retry delay after `failCount` consecutive failures: doubles the base
// interval per failure, capped at 60 s. Auth failures (401/403) should use
// AUTH_RETRY_MS directly — retrying a bad key faster is pointless.
export const MAX_BACKOFF_MS = 60000;
export const AUTH_RETRY_MS = 60000;
export function nextBackoff(failCount, baseMs) {
  const n = Math.max(0, Math.min(30, failCount | 0));
  return Math.min(MAX_BACKOFF_MS, Math.max(0, baseMs) * 2 ** n);
}
