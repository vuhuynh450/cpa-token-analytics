import React, { useEffect, useMemo, useRef, useState } from "react";
import { format } from "date-fns";
import { ChevronLeft, ChevronRight, Pencil, RefreshCw, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { SegmentedControl } from "@/components/segmented-control";
import { fetchFx, fetchPricing } from "../../services/api.js";
import { parsePrice } from "../../analytics.js";
import {
  fxEffectiveRate,
  fxSource,
  fxState,
  fxUpdatedAt,
  parseManualRate,
  rateAgeMinutes,
} from "../../fx.js";
import { translateBackend } from "../../i18n";
import { useI18n } from "../../i18n/react.jsx";
import {
  TabError,
  TabLoading,
  useErrorText,
  useTabData,
} from "../analytics/shared.jsx";

const fmtPrice = (v) =>
  typeof v === "number" && Number.isFinite(v) ? `$${v.toFixed(4)}` : "—";

const PAGE_SIZE = 25;

// source_used values from the contract ("models.dev" etc.) mapped to flat
// i18n keys — dots inside a value would break the dotted-key lookup.
const SOURCE_KEY = {
  "models.dev": "modelsDev",
  litellm: "litellm",
  cache: "cache",
  override: "override",
};

function StatusCard({ status, onSync, syncing, notice }) {
  const { t, fmt, locale } = useI18n();
  const item = (label, value) => (
    <div>
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-0.5 font-medium tabular-nums">{value}</p>
    </div>
  );
  return (
    <Card className="shadow-none">
      <CardHeader className="flex flex-wrap flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="text-base">{t("pricing.statusTitle")}</CardTitle>
          <CardDescription className="mt-1.5">
            {status?.enabled
              ? t("pricing.statusOn")
              : t("pricing.statusOff")}
          </CardDescription>
        </div>
        <div className="flex items-center gap-2">
          {status?.syncing && (
            <Badge variant="secondary">{t("pricing.syncing")}</Badge>
          )}
          <Button
            variant="outline"
            size="sm"
            onClick={onSync}
            disabled={syncing || status?.syncing}
          >
            <RefreshCw className={syncing ? "animate-spin" : ""} />
            {t("pricing.syncNow")}
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {notice && (
          <Alert variant={notice.ok ? "default" : "destructive"}>
            <AlertTitle>{t("pricing.syncResult")}</AlertTitle>
            <AlertDescription>
              {notice.ok
                ? t("pricing.syncOk", {
                    source: notice.source || "—",
                    models: fmt(notice.models),
                  })
                : translateBackend(notice.error || "", locale)}
            </AlertDescription>
          </Alert>
        )}
        {status?.last_error && (
          <Alert variant="destructive">
            <AlertTitle>{t("pricing.lastError")}</AlertTitle>
            <AlertDescription>
              {translateBackend(status.last_error, locale)}
            </AlertDescription>
          </Alert>
        )}
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
          {item(
            t("pricing.lastSync"),
            status?.last_sync
              ? format(new Date(status.last_sync), "yyyy-MM-dd HH:mm")
              : t("pricing.never"),
          )}
          {item(t("pricing.source"), status?.last_source || "—")}
          {item(
            t("pricing.sourceUsed"),
            status?.source_used
              ? t(`pricing.sourceName.${SOURCE_KEY[status.source_used] || "other"}`)
              : "—",
          )}
          {item(
            t("pricing.catalog"),
            `${fmt(status?.catalog_count)} ${t("pricing.modelsUnit")}`,
          )}
          {item(
            t("pricing.matched"),
            `${fmt(status?.matched_count)} ${t("pricing.modelsUnit")}`,
          )}
          {item(
            t("pricing.coverage"),
            status?.coverage_pct == null
              ? "—"
              : `${status.coverage_pct}%`,
          )}
        </div>
        {!!status?.unmatched_models?.length && (
          <p className="text-xs text-muted-foreground">
            {t("pricing.unmatchedList", {
              list: status.unmatched_models
                .map((m) => m.model || m)
                .join(", "),
            })}
          </p>
        )}
        {!!status?.ambiguous_models?.length && (
          <p className="text-xs text-muted-foreground">
            {t("pricing.ambiguousList", {
              list: status.ambiguous_models
                .map((m) =>
                  m.provider ? `${m.provider}/${m.model}` : m.model || m,
                )
                .join(", "),
            })}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

const FX_STATE_BADGE = {
  fresh: { variant: "secondary", key: "fx.upToDate" },
  cached: { variant: "outline", key: "fx.cachedRate" },
  syncing: { variant: "secondary", key: "fx.syncing" },
  failed: { variant: "destructive", key: "fx.syncFailed" },
  unavailable: { variant: "destructive", key: "fx.unavailable" },
};

// Relative "x ago" text for the cached-rate note — kept tiny and localized
// instead of pulling in date-fns distance helpers.
function rateAgeText(minutes, t, fmt) {
  if (minutes == null) return "";
  if (minutes < 1) return t("fx.cachedJustNow");
  if (minutes < 60) return t("fx.cachedMinutesAgo", { n: fmt(minutes) });
  const h = Math.floor(minutes / 60);
  if (h < 24) return t("fx.cachedHoursAgo", { n: fmt(h) });
  return t("fx.cachedDaysAgo", { n: fmt(Math.floor(h / 24)) });
}

// Exchange rate card: automatic USD→VND sync (ExchangeRate-API with a Fawaz
// fallback on the backend) or a user-entered manual rate. Reads
// /api/fx/status, mutations go to /api/fx/settings + /api/fx/sync.
function FxCard({ refetch, onError }) {
  const { t, fmt, locale } = useI18n();
  const errorText = useErrorText();
  const [version, setVersion] = useState(0);
  const [syncing, setSyncing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [fxText, setFxText] = useState("");
  const fxDirty = useRef(false);
  const [modeUi, setModeUi] = useState(null);

  const { data: fx, error, loading } = useTabData(() => fetchFx("status"), [
    refetch,
    version,
  ]);
  const mutated = () => setVersion((v) => v + 1);

  const mode = modeUi ?? fx?.mode ?? "auto";
  const rate = fxEffectiveRate(fx);
  const state = fxState(fx);
  const source = fxSource(fx);
  const updated = fxUpdatedAt(fx);
  const badge = FX_STATE_BADGE[state] || FX_STATE_BADGE.unavailable;
  const ageMin = rateAgeMinutes(fx?.fetched_at ?? updated);

  useEffect(() => {
    if (modeUi && fx?.mode === modeUi) setModeUi(null);
  }, [fx, modeUi]);

  // Keep the manual input in sync with the stored rate until the user edits.
  useEffect(() => {
    if (!fx || fxDirty.current) return;
    const seed = fx.manual_rate ?? (fx.mode === "manual" ? fx.rate : null);
    setFxText(typeof seed === "number" && Number.isFinite(seed) ? String(seed) : "");
  }, [fx]);

  async function syncNow() {
    setSyncing(true);
    try {
      await fetchFx("sync", { method: "POST" });
      mutated();
    } catch (e) {
      onError(e.message);
    } finally {
      setSyncing(false);
    }
  }

  async function setMode(v) {
    setModeUi(v);
    try {
      await fetchFx("settings", { method: "POST", body: { mode: v } });
      fxDirty.current = false;
      mutated();
    } catch (e) {
      setModeUi(null);
      onError(e.message);
    }
  }

  async function saveManual() {
    setSaving(true);
    try {
      const n = parseManualRate(fxText);
      if (n === null) {
        onError("invalid-fx");
        return;
      }
      await fetchFx("settings", {
        method: "POST",
        body: { mode: "manual", manual_rate: n },
      });
      fxDirty.current = false;
      mutated();
    } catch (e) {
      onError(e.message);
    } finally {
      setSaving(false);
    }
  }

  const meta = (label, value) => (
    <div>
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-0.5 font-medium tabular-nums">{value}</p>
    </div>
  );

  return (
    <Card className="shadow-none">
      <CardHeader className="flex flex-wrap flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="text-base">{t("fx.title")}</CardTitle>
          <CardDescription className="mt-1.5">{t("fx.pair")}</CardDescription>
          <p className="mt-1 text-xl font-semibold tabular-nums">
            {rate > 0 ? t("fx.rate", { rate: fmt(rate) }) : t("fx.unavailable")}
          </p>
        </div>
        <div className="flex flex-col items-end gap-2">
          <SegmentedControl
            ariaLabel={t("fx.modeAria")}
            value={mode}
            onChange={setMode}
            items={[
              { label: t("fx.automatic"), value: "auto" },
              { label: t("fx.manual"), value: "manual" },
            ]}
          />
          {mode === "auto" && (
            <Button
              variant="outline"
              size="sm"
              onClick={syncNow}
              disabled={syncing || fx?.syncing}
            >
              <RefreshCw
                className={syncing || fx?.syncing ? "animate-spin" : ""}
              />
              {syncing || fx?.syncing ? t("fx.syncing") : t("fx.syncNow")}
            </Button>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading && !fx && (
          <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        )}
        {error && (
          <Alert variant="destructive">
            <AlertTitle>{t("fx.unavailable")}</AlertTitle>
            <AlertDescription>{errorText(error)}</AlertDescription>
          </Alert>
        )}
        {!!fx?.last_error && (
          <Alert variant="destructive">
            <AlertTitle>{t("fx.syncFailed")}</AlertTitle>
            <AlertDescription>
              {translateBackend(fx.last_error, locale)}
            </AlertDescription>
          </Alert>
        )}
        {fx && (
          <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
            {meta(
              t("fx.source"),
              source ? (source === "manual" ? t("fx.sourceManual") : source) : "—",
            )}
            {meta(
              t("fx.lastUpdated"),
              updated ? format(updated, "yyyy-MM-dd HH:mm") : t("fx.never"),
            )}
            <div>
              <p className="text-xs text-muted-foreground">{t("fx.status")}</p>
              <p className="mt-0.5">
                <Badge variant={badge.variant}>{t(badge.key)}</Badge>
              </p>
            </div>
          </div>
        )}
        {mode === "auto" && fx?.last_error && rate > 0 && ageMin != null && (
          <p className="text-xs text-muted-foreground">
            {t("fx.cachedNote", { time: rateAgeText(ageMin, t, fmt) })}
          </p>
        )}
        {mode === "manual" && (
          <div className="space-y-2">
            <label
              htmlFor="fx-manual-rate"
              className="text-xs font-medium text-muted-foreground"
            >
              {t("fx.manualRate")}
            </label>
            <div className="flex items-center gap-2">
              <Input
                id="fx-manual-rate"
                type="number"
                min="0"
                step="any"
                className="w-40"
                placeholder={t("fx.placeholder")}
                value={fxText}
                onChange={(e) => {
                  fxDirty.current = true;
                  setFxText(e.target.value);
                }}
                onKeyDown={(e) => e.key === "Enter" && saveManual()}
              />
              <Button
                variant="outline"
                size="sm"
                onClick={saveManual}
                disabled={saving}
              >
                {t("fx.save")}
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">{t("fx.manualHint")}</p>
          </div>
        )}
        {source && source !== "manual" && (
          <p className="text-xs text-muted-foreground">
            {t("fx.attribution", { source })}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

export function PricingSettings({ refetch }) {
  const { t, fmt } = useI18n();
  const errorText = useErrorText();
  const [version, setVersion] = useState(0);
  const [syncing, setSyncing] = useState(false);
  const [notice, setNotice] = useState(null);
  const [edit, setEdit] = useState(null);
  const [editErr, setEditErr] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState("");
  const [page, setPage] = useState(0);

  const { data: status, error: statusErr } = useTabData(
    () => fetchPricing("status"),
    [version, refetch],
  );
  const { data: models, error: modelsErr, loading } = useTabData(
    () => fetchPricing("models"),
    [version, refetch],
  );

  const rows = useMemo(
    () =>
      [...(models?.models || [])].sort(
        (a, b) => (b.usage_requests || 0) - (a.usage_requests || 0),
      ),
    [models],
  );

  useEffect(() => setPage(0), [models]);
  const maxPage = Math.max(0, Math.ceil(rows.length / PAGE_SIZE) - 1),
    shown = rows.slice(
      Math.min(page, maxPage) * PAGE_SIZE,
      (Math.min(page, maxPage) + 1) * PAGE_SIZE,
    );

  async function syncNow() {
    setSyncing(true);
    setNotice(null);
    try {
      const d = await fetchPricing("sync", { method: "POST" });
      setNotice(d?.ok ? { ok: true, source: d.source, models: d.models } : { ok: false, error: d?.error });
    } catch (e) {
      setNotice({ ok: false, error: e.message });
    } finally {
      setSyncing(false);
      setVersion((v) => v + 1);
    }
  }

  async function setAutoSync(v) {
    try {
      await fetchPricing("settings", {
        method: "POST",
        body: { auto_sync: v === "on" },
      });
      setVersion((n) => n + 1);
    } catch (e) {
      setSaveErr(e.message);
    }
  }

  async function saveOverride(row) {
    setSaving(true);
    setSaveErr("");
    try {
      const body = { model: row.model, provider: row.provider || "" };
      // parsePrice tri-state: undefined = blank → omit field; null = invalid
      // → block the request; a number (0 is valid "free") goes into the body.
      for (const f of ["input", "output", "cache_read", "cache_write"]) {
        const v = parsePrice(edit[f]);
        if (v === null) {
          setEditErr(true);
          return;
        }
        if (v !== undefined) body[f] = v;
      }
      await fetchPricing("override", { method: "POST", body });
      setEdit(null);
      setEditErr(false);
      setVersion((v) => v + 1);
    } catch (e) {
      setSaveErr(e.message);
    } finally {
      setSaving(false);
    }
  }

  async function deleteOverride(row) {
    setSaving(true);
    setSaveErr("");
    try {
      await fetchPricing("override", {
        method: "POST",
        body: { model: row.model, provider: row.provider || "", delete: true },
      });
      setEdit(null);
      setEditErr(false);
      setVersion((v) => v + 1);
    } catch (e) {
      setSaveErr(e.message);
    } finally {
      setSaving(false);
    }
  }

  const editing = (row) => edit && edit.model === row.model && edit.provider === row.provider;
  const startEdit = (row) => {
    setEditErr(false);
    setEdit({
      model: row.model,
      provider: row.provider || "",
      input: row.prices?.input ?? "",
      output: row.prices?.output ?? "",
      cache_read: row.prices?.cache_read ?? "",
      cache_write: row.prices?.cache_write ?? "",
    });
  };

  if (loading && !models && !status) return <TabLoading />;
  return (
    <div className="space-y-6">
      <TabError error={statusErr || modelsErr} />
      {saveErr && (
        <Alert variant="destructive">
          <AlertTitle>{t("pricing.saveError")}</AlertTitle>
          <AlertDescription>
            {saveErr === "invalid-fx"
              ? t("fx.invalid")
              : errorText(saveErr)}
          </AlertDescription>
        </Alert>
      )}
      <StatusCard
        status={status}
        onSync={syncNow}
        syncing={syncing}
        notice={notice}
      />
      <FxCard refetch={refetch} onError={setSaveErr} />
      <Card className="shadow-none">
        <CardHeader>
          <CardTitle className="text-base">{t("pricing.settingsTitle")}</CardTitle>
          <CardDescription className="mt-1.5">
            {t("pricing.settingsHint")}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-end gap-x-8 gap-y-4">
          <div className="space-y-2">
            <p className="text-xs font-medium text-muted-foreground">
              {t("pricing.autoSync")}
            </p>
            <SegmentedControl
              ariaLabel={t("pricing.autoSyncAria")}
              value={status?.enabled === false ? "off" : "on"}
              onChange={setAutoSync}
              items={[
                { label: t("pricing.on"), value: "on" },
                { label: t("pricing.off"), value: "off" },
              ]}
            />
          </div>
        </CardContent>
      </Card>
      <Card className="gap-0 overflow-hidden shadow-none">
        <CardHeader className="pb-4">
          <CardTitle className="text-base">{t("pricing.modelsTitle")}</CardTitle>
          <CardDescription className="mt-1.5">
            {t("pricing.modelsHint")}
          </CardDescription>
        </CardHeader>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-6">{t("pricing.col.model")}</TableHead>
                <TableHead>{t("pricing.col.provider")}</TableHead>
                <TableHead>{t("pricing.col.source")}</TableHead>
                <TableHead className="text-right">{t("pricing.col.input")}</TableHead>
                <TableHead className="text-right">{t("pricing.col.cacheRead")}</TableHead>
                <TableHead className="text-right">{t("pricing.col.cacheWrite")}</TableHead>
                <TableHead className="text-right">{t("pricing.col.output")}</TableHead>
                <TableHead>{t("pricing.col.matchedBy")}</TableHead>
                <TableHead className="text-right">{t("pricing.col.usage")}</TableHead>
                <TableHead className="pr-6 text-right">{t("pricing.col.actions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) =>
                editing(r) ? (
                  <React.Fragment key={`${r.provider}/${r.model}`}>
                    <TableRow>
                      <TableCell className="max-w-48 truncate pl-6 font-medium" title={r.model}>
                        {r.model}
                      </TableCell>
                      <TableCell>{r.provider || "—"}</TableCell>
                      <TableCell>
                        <Badge variant="outline">{r.source || "—"}</Badge>
                      </TableCell>
                      {["input", "cache_read", "cache_write", "output"].map((f) => (
                        <TableCell key={f} className="text-right">
                          <Input
                            type="number"
                            min="0"
                            step="any"
                            className="ml-auto h-7 w-24 text-right text-xs"
                            value={edit[f]}
                            aria-label={t(`pricing.col.${f === "cache_read" ? "cacheRead" : f === "cache_write" ? "cacheWrite" : f}`)}
                            onChange={(e) => {
                              setEditErr(false);
                              setEdit({ ...edit, [f]: e.target.value });
                            }}
                          />
                        </TableCell>
                      ))}
                      <TableCell>{r.matched_by || "—"}</TableCell>
                      <TableCell className="text-right tabular-nums">
                        {fmt(r.usage_requests)}
                      </TableCell>
                      <TableCell className="pr-6 text-right">
                        <div className="flex justify-end gap-1">
                          <Button
                            size="xs"
                            onClick={() => saveOverride(r)}
                            disabled={saving}
                          >
                            {t("pricing.save")}
                          </Button>
                          <Button
                            size="xs"
                            variant="ghost"
                            onClick={() => {
                              setEdit(null);
                              setEditErr(false);
                            }}
                            disabled={saving}
                          >
                            {t("pricing.cancel")}
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                    {editErr && (
                      <TableRow className="border-b-0 hover:bg-transparent">
                        <TableCell
                          colSpan={10}
                          className="pb-2 pt-0 text-right text-xs text-destructive"
                        >
                          {t("pricing.overrideInvalid")}
                        </TableCell>
                      </TableRow>
                    )}
                  </React.Fragment>
                ) : (
                  <TableRow key={`${r.provider}/${r.model}`}>
                    <TableCell className="max-w-48 truncate pl-6 font-medium" title={r.model}>
                      {r.model}
                    </TableCell>
                    <TableCell>{r.provider || "—"}</TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          r.source === "override" ? "default" : "outline"
                        }
                      >
                        {r.source || "—"}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmtPrice(r.prices?.input)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmtPrice(r.prices?.cache_read)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmtPrice(r.prices?.cache_write)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmtPrice(r.prices?.output)}
                    </TableCell>
                    <TableCell>{r.matched_by || "—"}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmt(r.usage_requests)}
                    </TableCell>
                    <TableCell className="pr-6 text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          size="icon-xs"
                          variant="ghost"
                          aria-label={t("pricing.edit")}
                          onClick={() => startEdit(r)}
                        >
                          <Pencil />
                        </Button>
                        {(r.source === "override" || r.matched_by === "override") && (
                          <Button
                            size="icon-xs"
                            variant="ghost"
                            className="text-destructive"
                            aria-label={t("pricing.delete")}
                            onClick={() => deleteOverride(r)}
                            disabled={saving}
                          >
                            <Trash2 />
                          </Button>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                ),
              )}
              {!rows.length && (
                <TableRow>
                  <TableCell
                    colSpan={10}
                    className="h-32 text-center text-muted-foreground"
                  >
                    {loading ? t("details.loading") : t("pricing.modelsEmpty")}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
        <div className="flex items-center justify-between border-t px-6 py-3 text-xs text-muted-foreground">
          <span>{t("pricing.perMillion")}</span>
          {rows.length > PAGE_SIZE && (
            <div className="flex items-center gap-2">
              <Button
                variant="ghost"
                size="icon"
                className="size-7"
                aria-label={t("details.prevPage")}
                disabled={page === 0}
                onClick={() => setPage((p) => p - 1)}
              >
                <ChevronLeft />
              </Button>
              <span>
                {Math.min(page, maxPage) + 1} / {maxPage + 1}
              </span>
              <Button
                variant="ghost"
                size="icon"
                className="size-7"
                aria-label={t("details.nextPage")}
                disabled={page >= maxPage}
                onClick={() => setPage((p) => p + 1)}
              >
                <ChevronRight />
              </Button>
            </div>
          )}
        </div>
      </Card>
    </div>
  );
}
