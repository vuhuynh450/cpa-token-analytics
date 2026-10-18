import React, { useEffect, useMemo, useRef, useState } from "react";
import { Bell, ChevronLeft, ChevronRight } from "lucide-react";
import { format } from "date-fns";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { fetchRequests } from "../../services/api.js";
import { dateKey } from "../../analytics.js";
import { useI18n } from "../../i18n/react.jsx";
import { TabError } from "../analytics/shared.jsx";
import {
  DEFAULT_FILTERS,
  PAGE_SIZES,
  DEFAULT_PAGE_SIZE,
  buildRequestParams,
  cursorAt,
  pageOf,
  popCursor,
  pushCursor,
} from "../../requests.js";
import { RequestFilters } from "./request-filters.jsx";
import { RequestTable } from "./request-table.jsx";
import { RequestDetail } from "./request-detail.jsx";

// Debounce: text inputs (search/min_*) would otherwise hit the API per
// keystroke; select/sort/page changes absorb a 250 ms delay harmlessly.
const DEBOUNCE_MS = 250;

// The local model filter narrows the shared header filter: "all" inherits
// the global selection, a specific model overrides it for this tab.
function effectiveModel(local, global) {
  return local && local !== "all" ? local : global || "all";
}

export function RequestInspector({ range, model, models, refetch, live }) {
  const { t, fmt } = useI18n();
  const [filters, setFilters] = useState(DEFAULT_FILTERS);
  const [limit, setLimit] = useState(DEFAULT_PAGE_SIZE);
  // Cursor stack: cursors[i] is the cursor used to fetch page i+2, so the
  // visible page number is cursors.length + 1 and the current cursor is the
  // stack top (undefined on page 1).
  const [cursors, setCursors] = useState([]);
  const [params, setParams] = useState(null);
  const [viewId, setViewId] = useState(null);
  const [detailOpen, setDetailOpen] = useState(false);
  // Live bumps: null = not shown, ts = new rows exist since <ts>.
  const [pendingNew, setPendingNew] = useState(null);

  const merged = useMemo(
    () => ({
      ...filters,
      limit,
      start: dateKey(range.from),
      end: dateKey(range.to),
      model: effectiveModel(filters.model, model),
    }),
    [filters, limit, range, model],
  );

  // Any filter/sort/limit/range change restarts cursor pagination at page 1.
  useEffect(() => {
    setCursors([]);
  }, [merged]);

  useEffect(() => {
    const timer = setTimeout(
      () => setParams(buildRequestParams(merged, cursorAt(cursors))),
      DEBOUNCE_MS,
    );
    return () => clearTimeout(timer);
  }, [merged, cursors]);

  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const generation = useRef(0);
  // What the last processed (refetch, live) pair was — a refetch bump that
  // arrives together with a live bump is a live refresh, not a manual one.
  const lastSeen = useRef({ refetch, live });
  // Live bumps on the head-of-list view fetch softly: current rows stay on
  // screen (no skeleton flash); the footer still shows `loading && data`.
  const softFetch = useRef(false);

  // Decides what a live bump means BEFORE the fetch effect runs (effects
  // execute top-down): page 1 + newest sort + no open detail → flag a soft
  // refetch; any other view → just show the "new requests" pill, since the
  // keyset cursor keeps the fetched page valid under inserts.
  const livePrev = useRef(live);
  useEffect(() => {
    if (live === livePrev.current) return;
    livePrev.current = live;
    // null = mount baseline, nothing new yet; 0 is a valid revision.
    if (live == null) return;
    if (!cursors.length && filters.sort === "newest" && !detailOpen) {
      softFetch.current = true;
      setPendingNew(null);
    } else {
      setPendingNew(Date.now());
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live]);

  useEffect(() => {
    const liveBump = lastSeen.current.live !== live;
    lastSeen.current = { refetch, live };
    if (!params) return undefined;
    // Live bump on a non-head view: pill already shown — leave the page
    // exactly as the user left it (no fetch, no detail interference).
    if (liveBump && !softFetch.current) return undefined;
    const gen = ++generation.current;
    const ac = new AbortController();
    const soft = softFetch.current;
    softFetch.current = false;
    if (!soft) setLoading(true);
    fetchRequests(params, { signal: ac.signal })
      .then((d) => {
        if (gen === generation.current) {
          setData(d);
          setError("");
          setPendingNew(null); // fresh rows on screen — hint resolved
        }
      })
      .catch((e) => {
        if (gen === generation.current && e.name !== "AbortError") {
          setError(e.message);
        }
      })
      .finally(() => {
        if (gen === generation.current) setLoading(false);
      });
    return () => {
      generation.current++;
      ac.abort();
    };
  }, [params, refetch, live]);

  // Any param change (filter/sort/page applied by the user) already fetches
  // fresh rows, so a pending "new requests" hint is always resolved.
  useEffect(() => setPendingNew(null), [params]);

  const items = data?.items || [];
  const page = pageOf(cursors);
  const openDetail = (id) => {
    if (id == null) return;
    setViewId(id);
    setDetailOpen(true);
  };

  return (
    <div className="space-y-4">
      <TabError error={error} />
      <Card className="gap-0 overflow-hidden shadow-none">
        <CardHeader className="pb-4">
          <CardTitle className="text-base">{t("req.title")}</CardTitle>
          <CardDescription className="mt-1.5">
            {t("req.description")}
          </CardDescription>
        </CardHeader>
        <div className="border-t px-6 py-4">
          <RequestFilters
            filters={filters}
            onChange={setFilters}
            models={models}
          />
        </div>
        <div className="overflow-x-auto border-t">
          <RequestTable
            items={items}
            loading={loading}
            sort={filters.sort}
            onSort={(s) => setFilters((f) => ({ ...f, sort: s }))}
            onView={openDetail}
          />
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t px-6 py-3 text-xs text-muted-foreground">
          <span aria-live="polite">
            {t("req.pageInfo", {
              page: fmt(page),
              count: fmt(items.length),
            })}
            {data?.has_more ? ` · ${t("req.hasMore")}` : ""}
            {loading && data ? ` · ${t("req.loading")}` : ""}
          </span>
          {pendingNew !== null && (
            <Button
              variant="secondary"
              size="sm"
              className="h-6 gap-1.5 px-2.5 text-xs"
              title={t("live.lastData", {
                time: format(new Date(pendingNew), "HH:mm:ss"),
              })}
              onClick={() => {
                // Back to the live head: page 1 + newest-first.
                setFilters((f) => ({ ...f, sort: "newest" }));
                setCursors([]);
                setPendingNew(null);
              }}
            >
              <Bell className="size-3" />
              {t("live.newRequests")}
            </Button>
          )}
          <div className="flex items-center gap-2">
            <Select
              value={String(limit)}
              onValueChange={(v) => setLimit(Number(v))}
            >
              <SelectTrigger size="sm" aria-label={t("req.perPage")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {PAGE_SIZES.map((n) => (
                  <SelectItem key={n} value={String(n)}>
                    {t("req.perPage", { n })}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button
              variant="ghost"
              size="icon"
              className="size-7"
              aria-label={t("req.prevPage")}
              disabled={!cursors.length}
              onClick={() => setCursors((c) => popCursor(c))}
            >
              <ChevronLeft />
            </Button>
            <span>{fmt(page)}</span>
            <Button
              variant="ghost"
              size="icon"
              className="size-7"
              aria-label={t("req.nextPage")}
              disabled={!data?.has_more || !data?.next_cursor}
              onClick={() =>
                setCursors((c) => pushCursor(c, data.next_cursor))
              }
            >
              <ChevronRight />
            </Button>
          </div>
        </div>
      </Card>
      <RequestDetail
        id={viewId}
        open={detailOpen}
        onOpenChange={setDetailOpen}
      />
    </div>
  );
}
