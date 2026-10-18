import { useCallback, useEffect, useRef, useState } from "react";
import { fetchJSON, KEY_PROMPT } from "../services/api.js";
import {
  AUTH_RETRY_MS,
  decideAction,
  decodeChanges,
  LIVE_INTERVALS,
  parseLiveMode,
  nextBackoff,
  readLiveMode,
  writeLiveMode,
} from "../live.js";

// Central near-realtime scheduler: polls GET /api/changes on a setTimeout
// chain (never setInterval — no overlap) and calls onNewData() only when
// the (dataset_id, usage_revision) tuple actually moves. First successful
// poll just records the baseline, so it cannot double-fire the initial
// tab load.
export function useLiveUpdates({ onNewData } = {}) {
  const [mode, setModeState] = useState(() =>
    readLiveMode((k) => localStorage.getItem(k)),
  );
  const [status, setStatus] = useState("live");
  const [lastDataAt, setLastDataAt] = useState(null);
  const [lastCheckAt, setLastCheckAt] = useState(null);
  const [revision, setRevision] = useState(null);

  const cbRef = useRef(onNewData);
  cbRef.current = onNewData;
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const baseline = useRef({ revision: null, datasetId: null });
  const fails = useRef(0);
  const inFlight = useRef(null);
  const timer = useRef(null);
  // De-dupe queued state writes so unchanged values skip the render.
  const committed = useRef({ status: null, lastCheckAt: null });
  const commit = (patch) => {
    const c = committed.current;
    if (patch.status !== undefined && patch.status !== c.status) {
      c.status = patch.status;
      setStatus(patch.status);
    }
    if (
      patch.lastCheckAt !== undefined &&
      patch.lastCheckAt !== c.lastCheckAt
    ) {
      c.lastCheckAt = patch.lastCheckAt;
      setLastCheckAt(patch.lastCheckAt);
    }
  };

  const tick = useCallback(async () => {
    // A pending retry/backoff timer must never coexist with a live tick.
    if (timer.current) {
      clearTimeout(timer.current);
      timer.current = null;
    }
    if (inFlight.current || document.hidden || !navigator.onLine) return;
    const ac = new AbortController();
    inFlight.current = ac;
    try {
      const json = await fetchJSON("/changes", { signal: ac.signal });
      if (ac.signal.aborted) return;
      fails.current = 0;
      const parsed = decodeChanges(json);
      const action = decideAction(baseline.current, parsed);
      if (parsed) baseline.current = parsed;
      commit({ status: "live", lastCheckAt: Date.now() });
      if (action === "refresh") {
        setRevision(parsed.revision);
        setLastDataAt(Date.now());
        cbRef.current?.();
      }
    } catch (e) {
      if (ac.signal.aborted) return;
      const delay =
        e?.message === KEY_PROMPT
          ? AUTH_RETRY_MS
          : nextBackoff(fails.current, LIVE_INTERVALS[modeRef.current] || 3000);
      fails.current += 1;
      // Auth failures retry on a flat 60 s; everything else backs off.
      commit({ status: "reconnecting" });
      timer.current = setTimeout(tick, delay);
      return;
    } finally {
      inFlight.current = null;
    }
    schedule();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const schedule = useCallback(() => {
    if (timer.current) clearTimeout(timer.current);
    timer.current = null;
    const ms = LIVE_INTERVALS[modeRef.current];
    if (ms > 0 && !document.hidden && navigator.onLine) {
      timer.current = setTimeout(tick, ms);
    }
  }, [tick]);

  const setMode = useCallback(
    (m) => {
      const next = parseLiveMode(m);
      modeRef.current = next;
      setModeState(next);
      writeLiveMode(next, (k, v) => localStorage.setItem(k, v));
      if (next === "paused") {
        if (timer.current) clearTimeout(timer.current);
        timer.current = null;
        inFlight.current?.abort();
        commit({ status: "paused" });
      } else {
        fails.current = 0;
        commit({ status: "live" });
        tick(); // immediate poll on resume, then self-reschedules
      }
      // eslint-disable-next-line react-hooks/exhaustive-deps
    },
    [schedule],
  );

  // Manual resync: poll once now to realign the baseline (manual Refresh
  // must not be mistaken for new data on the next tick).
  const pollNow = useCallback(() => {
    tick();
  }, [tick]);

  useEffect(() => {
    const onVis = () => {
      if (document.hidden) {
        if (timer.current) clearTimeout(timer.current);
        timer.current = null;
        inFlight.current?.abort();
      } else {
        fails.current = 0;
        if (navigator.onLine) {
          commit({ status: "live" });
          tick(); // poll immediately, then resume the interval
        }
      }
    };
    const onOffline = () => {
      if (timer.current) clearTimeout(timer.current);
      timer.current = null;
      inFlight.current?.abort();
      commit({ status: "offline" });
    };
    const onOnline = () => {
      fails.current = 0;
      if (!document.hidden) {
        commit({ status: "live" });
        tick();
      }
    };
    document.addEventListener("visibilitychange", onVis);
    window.addEventListener("offline", onOffline);
    window.addEventListener("online", onOnline);
    if (!navigator.onLine) commit({ status: "offline" });
    else if (modeRef.current === "paused") commit({ status: "paused" });
    else tick(); // establish the baseline right away; ticks self-reschedule
    return () => {
      if (timer.current) clearTimeout(timer.current);
      timer.current = null;
      inFlight.current?.abort();
      document.removeEventListener("visibilitychange", onVis);
      window.removeEventListener("offline", onOffline);
      window.removeEventListener("online", onOnline);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [schedule, tick]);

  return {
    mode,
    setMode,
    status,
    lastDataAt,
    lastCheckAt,
    revision,
    pollNow,
  };
}
