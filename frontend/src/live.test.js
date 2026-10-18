import test from "node:test";
import assert from "node:assert/strict";
import {
  AUTH_RETRY_MS,
  decodeChanges,
  decideAction,
  DEFAULT_LIVE_MODE,
  LIVE_INTERVALS,
  LIVE_MODES,
  LIVE_STORAGE_KEY,
  MAX_BACKOFF_MS,
  nextBackoff,
  parseLiveMode,
  readLiveMode,
  writeLiveMode,
} from "./live.js";

const resp = (revision = 1, dataset_id = "ds-1") => ({
  usage_revision: revision,
  count: 42,
  dataset_id,
  checked_at: "2026-10-18T09:00:00Z",
});

test("decodeChanges accepts the contract shape", () => {
  assert.deepEqual(decodeChanges(resp(7, "abc")), {
    revision: 7,
    datasetId: "abc",
  });
});

test("decodeChanges rejects malformed payloads", () => {
  for (const bad of [
    null,
    undefined,
    "x",
    5,
    {},
    { usage_revision: "1", dataset_id: "d" },
    { usage_revision: 1 },
    { usage_revision: 1, dataset_id: "" },
    { usage_revision: NaN, dataset_id: "d" },
    { usage_revision: Infinity, dataset_id: "d" },
    { usage_revision: 1, dataset_id: 9 },
  ]) {
    assert.equal(decodeChanges(bad), null, JSON.stringify(bad));
  }
});

test("decideAction: first poll is baseline, never refresh", () => {
  assert.equal(decideAction(null, decodeChanges(resp(5))), "baseline");
  assert.equal(
    decideAction({ revision: null, datasetId: null }, decodeChanges(resp(5))),
    "baseline",
  );
  assert.equal(
    decideAction(
      { revision: null, datasetId: "d" },
      decodeChanges(resp(5)),
    ),
    "baseline",
  );
});

test("decideAction: same tuple is none, new revision is refresh", () => {
  const prev = { revision: 3, datasetId: "ds-1" };
  assert.equal(decideAction(prev, decodeChanges(resp(3))), "none");
  assert.equal(decideAction(prev, decodeChanges(resp(4))), "refresh");
  // Revision moving backwards still means the data changed (e.g. reset).
  assert.equal(decideAction(prev, decodeChanges(resp(2))), "refresh");
});

test("decideAction: dataset_id change always refreshes", () => {
  const prev = { revision: 3, datasetId: "ds-1" };
  assert.equal(decideAction(prev, decodeChanges(resp(3, "ds-2"))), "refresh");
  assert.equal(decideAction(prev, decodeChanges(resp(9, "ds-2"))), "refresh");
});

test("decideAction: malformed response falls back to baseline", () => {
  const prev = { revision: 3, datasetId: "ds-1" };
  assert.equal(decideAction(prev, null), "baseline");
  assert.equal(decideAction(null, null), "baseline");
});

test("nextBackoff doubles per failure and caps at 60 s", () => {
  assert.equal(nextBackoff(0, 3000), 3000);
  assert.equal(nextBackoff(1, 3000), 6000);
  assert.equal(nextBackoff(2, 3000), 12000);
  assert.equal(nextBackoff(3, 3000), 24000);
  assert.equal(nextBackoff(4, 3000), 48000);
  assert.equal(nextBackoff(5, 3000), 60000);
  assert.equal(nextBackoff(9, 3000), 60000);
  assert.equal(MAX_BACKOFF_MS, 60000);
  // Auth failures use the flat slow retry, not the doubling ladder.
  assert.equal(AUTH_RETRY_MS, 60000);
});

test("nextBackoff clamps weird inputs", () => {
  assert.equal(nextBackoff(-2, 3000), 3000);
  assert.equal(nextBackoff(1.7, 3000), 6000);
  assert.equal(nextBackoff(100, 3000), 60000);
  assert.equal(nextBackoff(0, 0), 0);
});

test("parseLiveMode round-trips modes, invalid falls back to default", () => {
  for (const m of LIVE_MODES) assert.equal(parseLiveMode(m), m);
  for (const bad of ["", "live1", "LIVE3", null, undefined, 3, {}]) {
    assert.equal(parseLiveMode(bad), DEFAULT_LIVE_MODE, String(bad));
  }
  assert.equal(parseLiveMode(DEFAULT_LIVE_MODE), "live3");
});

test("read/writeLiveMode persist through the injected storage", () => {
  const store = new Map();
  const getItem = (k) => (store.has(k) ? store.get(k) : null);
  const setItem = (k, v) => store.set(k, v);
  writeLiveMode("live10", setItem);
  assert.equal(store.get(LIVE_STORAGE_KEY), "live10");
  assert.equal(readLiveMode(getItem), "live10");
  store.set(LIVE_STORAGE_KEY, "garbage");
  assert.equal(readLiveMode(getItem), DEFAULT_LIVE_MODE);
  assert.equal(readLiveMode(() => null), DEFAULT_LIVE_MODE);
  // Storage that throws must not break the caller.
  assert.equal(
    readLiveMode(() => {
      throw new Error("denied");
    }),
    DEFAULT_LIVE_MODE,
  );
  writeLiveMode("paused", () => {
    throw new Error("denied");
  });
});

test("LIVE_INTERVALS covers the UI modes and paused is 0", () => {
  assert.equal(LIVE_INTERVALS.live3, 3000);
  assert.equal(LIVE_INTERVALS.live5, 5000);
  assert.equal(LIVE_INTERVALS.live10, 10000);
  assert.equal(LIVE_INTERVALS.paused, 0);
  assert.deepEqual(LIVE_MODES, ["live3", "live5", "live10", "paused"]);
});

test("changes poll uses the shared fetchJSON (no credentials)", async () => {
  const src = await import("node:fs/promises").then((fs) =>
    fs.readFile(new URL("./services/api.js", import.meta.url), "utf8"),
  );
  // fetch options for /api/changes flow through fetchJSON; assert it never
  // sets credentials/mode so requests stay same-origin cookie-free.
  assert.ok(!/credentials\s*:/.test(src));
});
