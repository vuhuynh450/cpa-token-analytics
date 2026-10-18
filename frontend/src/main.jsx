import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Activity,
  ArrowDownUp,
  CalendarDays,
  Check,
  ChevronLeft,
  ChevronRight,
  Database,
  KeyRound,
  Layers,
  Moon,
  RefreshCw,
  Search,
  Sun,
  WifiOff,
} from "lucide-react";
import { format, subDays } from "date-fns";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { SegmentedControl } from "@/components/segmented-control";
import { LanguageSwitcher } from "@/components/language-switcher";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Calendar } from "@/components/ui/calendar";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { Separator } from "@/components/ui/separator";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { dateKey, groups, summarize, trend } from "./analytics";
import { translateBackend } from "./i18n";
import { I18nProvider, useI18n } from "./i18n/react.jsx";
import {
  API_BASE,
  fetchJSON,
  readManagementKey,
} from "./services/api.js";
import { Metric, useErrorText } from "./components/analytics/shared.jsx";
import { useLiveUpdates } from "./hooks/use-live-updates.js";
import { CostOverview } from "./components/analytics/cost-overview.jsx";
import { ModelPerformance } from "./components/analytics/model-performance.jsx";
import { UsageComparison } from "./components/analytics/usage-comparison.jsx";
import { ChartsTab } from "./components/analytics/charts-tab.jsx";
import { PricingSettings } from "./components/pricing/pricing-settings.jsx";
import { RequestInspector } from "./components/requests/request-inspector.jsx";
import "./index.css";

// Re-exported so analytics components can share the same auth/base helpers.
export { API_BASE, readManagementKey };

const initialRange = () => {
  const today = new Date();
  return { from: today, to: today };
};

const NAV_TABS = [
  "overview",
  "cost",
  "performance",
  "compare",
  "charts",
  "requests",
  "pricing",
];

// Which quick preset a stored date range corresponds to (1/7/30), or 0 for
// a custom range. Detection is date-based: the range must still end today.
const quickDays = (r) => {
  if (!r?.from || !r?.to || dateKey(r.to) !== dateKey(new Date())) return 0;
  const from = dateKey(r.from);
  if (from === dateKey(r.to)) return 1;
  if (from === dateKey(subDays(new Date(), 6))) return 7;
  if (from === dateKey(subDays(new Date(), 29))) return 30;
  return 0;
};

function App() {
  const { locale, t, dateFnsLocale, fmt, compact } = useI18n();
  const [range, setRange] = useState(initialRange),
    [draft, setDraft] = useState(initialRange),
    [calendarOpen, setCalendarOpen] = useState(false),
    [bucket, setBucket] = useState("hour"),
    [model, setModel] = useState("all"),
    [data, setData] = useState(null),
    [error, setError] = useState(""),
    [loading, setLoading] = useState(true),
    [busy, setBusy] = useState(false),
    [query, setQuery] = useState(""),
    [tab, setTab] = useState("model"),
    [nav, setNav] = useState("overview"),
    [refetch, setRefetch] = useState(0),
    [page, setPage] = useState(0),
    [sort, setSort] = useState("tokens"),
    [dark, setDark] = useState(
      () => localStorage.getItem("cpa-usage-theme") === "dark",
    );
  const generation = useRef(0);
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    localStorage.setItem("cpa-usage-theme", dark ? "dark" : "light");
  }, [dark]);
  async function load() {
    const id = ++generation.current;
    setBusy(true);
    try {
      const d = await fetchJSON(
        `/usage?start=${dateKey(range.from)}&end=${dateKey(range.to)}&bucket=${bucket}`,
      );
      if (id === generation.current) {
        setData(d);
        setError("");
      }
    } catch (e) {
      if (id === generation.current) setError(e.message);
    } finally {
      if (id === generation.current) {
        setBusy(false);
        setLoading(false);
      }
    }
  }
  // Live updates: a revision change refreshes the active view. When a
  // quick preset (Today/7d/30d) spans a midnight rollover, its stored dates
  // go stale — recompute them first so "today" keeps meaning today; custom
  // ranges are never rewritten.
  const onNewData = () => {
    if (dateKey(range.to) !== dateKey(new Date())) {
      setRange((r) => {
        const d = quickDays(r);
        return d ? { from: subDays(new Date(), d - 1), to: new Date() } : r;
      });
    }
    setRefetch((v) => v + 1);
  };
  const live = useLiveUpdates({ onNewData });
  useEffect(() => {
    load();
    return () => {
      generation.current++;
    };
  }, [range, bucket, refetch]);
  const events = useMemo(
    () =>
      (data?.events || []).filter((e) => model === "all" || e.model === model),
    [data, model],
  );
  const stats = useMemo(() => summarize(events), [events]),
    buckets = useMemo(() => trend(events, bucket), [events, bucket]);
  const modelNames = useMemo(
    () => [...new Set((data?.events || []).map((e) => e.model))].sort(),
    [data],
  );
  useEffect(() => {
    if (model !== "all" && !modelNames.includes(model)) setModel("all");
  }, [modelNames]);
  useEffect(() => setPage(0), [tab, query, model, range, sort]);
  const rows = useMemo(() => {
    let result =
      tab === "requests"
        ? [...events].reverse()
        : groups(events, tab === "model" ? "model" : "key");
    result = result.filter((r) =>
      (r.name || r.model + " " + r.key)
        .toLowerCase()
        .includes(query.toLowerCase()),
    );
    if (tab !== "requests")
      result.sort((a, b) =>
        sort === "requests"
          ? b.n - a.n
          : sort === "name"
            ? a.name.localeCompare(b.name)
            : b.input + b.output - (a.input + a.output),
      );
    return result;
  }, [events, tab, query, sort]);
  const maxPage = Math.max(0, Math.ceil(rows.length / 8) - 1),
    shown = rows.slice(
      Math.min(page, maxPage) * 8,
      (Math.min(page, maxPage) + 1) * 8,
    );
  const total = stats.input + stats.output,
    rate = stats.n
      ? (((stats.n - stats.fail) / stats.n) * 100).toFixed(1)
      : null;
  const healthy =
    data?.configured && data?.collected_at && !data?.collect_err && !error;
  const quick = (days) =>
    setRange({ from: subDays(new Date(), days - 1), to: new Date() });
  const activeQuick = quickDays(range);
  const chartConfig = useMemo(
    () => ({
      cache: { label: t("chart.cachedInput"), color: "var(--chart-1)" },
      net: { label: t("chart.netInput"), color: "var(--chart-2)" },
      output: { label: t("chart.output"), color: "var(--chart-3)" },
    }),
    [t],
  );
  const errorText = useErrorText();
  return (
    <TooltipProvider>
      <div className="min-h-dvh">
        <header className="border-b bg-card">
          <div className="mx-auto flex h-16 max-w-7xl items-center justify-between px-4 sm:px-8">
            <a
              href="/"
              className="flex items-center gap-3 font-semibold tracking-tight"
            >
              <span className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
                <Activity className="size-4" />
              </span>
              CPA Usage
              <Badge
                variant="outline"
                className="hidden sm:inline-flex font-normal text-muted-foreground"
              >
                LOCAL
              </Badge>
            </a>
            <div className="flex items-center gap-2">
              <Badge
                variant="outline"
                className="hidden sm:flex gap-1.5 font-normal"
              >
                {healthy ? (
                  <Check className="size-3 text-primary" />
                ) : (
                  <WifiOff className="size-3" />
                )}
                {healthy
                  ? t("header.statusHealthy")
                  : t("header.statusWaiting")}
              </Badge>
              <LanguageSwitcher />
              <Button
                variant="ghost"
                size="icon"
                aria-label={
                  dark ? t("header.themeToLight") : t("header.themeToDark")
                }
                onClick={() => setDark(!dark)}
              >
                {dark ? <Sun /> : <Moon />}
              </Button>
            </div>
          </div>
        </header>
        <main className="mx-auto max-w-7xl space-y-6 px-4 py-8 sm:px-8 sm:py-10">
          <div className="flex flex-wrap items-end justify-between gap-4">
            <div>
              <p className="mb-2 text-xs font-medium text-muted-foreground">
                {t("hero.crumb")}
              </p>
              <h1 className="text-3xl font-semibold tracking-tight">
                {t("hero.title")}
              </h1>
              <p className="mt-2 text-sm text-muted-foreground">
                {t("hero.subtitle")}
              </p>
            </div>
            <div className="flex flex-wrap items-center justify-end gap-2">
              <div className="flex items-center gap-2 rounded-md border px-2.5 py-1.5">
                <span
                  className={
                    "size-2 rounded-full " +
                    (live.status === "live"
                      ? "bg-primary"
                      : live.status === "paused"
                        ? "bg-muted-foreground"
                        : live.status === "offline"
                          ? "bg-destructive"
                          : "animate-pulse bg-amber-500")
                  }
                  title={`${t("live.title")}: ${t(`live.${live.status}`)}${
                    live.lastDataAt
                      ? ` · ${t("live.lastData", {
                          time: format(live.lastDataAt, "HH:mm:ss"),
                        })}`
                      : ""
                  }`}
                />
                <span className="sr-only">
                  {t("live.title")}: {t(`live.${live.status}`)}
                  {live.lastDataAt
                    ? ` · ${t("live.lastData", {
                        time: format(live.lastDataAt, "HH:mm:ss"),
                      })}`
                    : ""}
                </span>
                <Select value={live.mode} onValueChange={live.setMode}>
                  <SelectTrigger
                    size="sm"
                    aria-label={t("live.aria")}
                    className="h-6! w-auto gap-1 border-0 px-1 text-xs shadow-none dark:bg-transparent dark:hover:bg-transparent"
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent align="end">
                    <SelectItem value="live3">{t("live.every3")}</SelectItem>
                    <SelectItem value="live5">{t("live.every5")}</SelectItem>
                    <SelectItem value="live10">{t("live.every10")}</SelectItem>
                    <SelectItem value="paused">{t("live.paused")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <Button
                variant="outline"
                onClick={() => {
                  load();
                  setRefetch((n) => n + 1);
                  live.pollNow();
                }}
                disabled={busy}
              >
                <RefreshCw className={busy ? "animate-spin" : ""} />
                {busy ? t("actions.refreshing") : t("actions.refresh")}
              </Button>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <SegmentedControl
              ariaLabel={t("range.aria")}
              value={activeQuick}
              onChange={quick}
              items={[
                { label: t("range.today"), value: 1 },
                { label: t("range.last7"), value: 7 },
                { label: t("range.last30"), value: 30 },
              ]}
            />
            <Popover
              open={calendarOpen}
              onOpenChange={(o) => {
                setCalendarOpen(o);
                if (o) setDraft(range);
              }}
            >
              <PopoverTrigger asChild>
                <Button variant="outline" size="sm">
                  <CalendarDays />
                  {format(range.from, "yyyy/MM/dd")} –{" "}
                  {format(range.to, "MM/dd")}
                </Button>
              </PopoverTrigger>
              <PopoverContent align="start" className="w-auto p-0">
                <Calendar
                  mode="range"
                  locale={dateFnsLocale}
                  selected={draft}
                  onSelect={(v) => setDraft(v || {})}
                  defaultMonth={range.from}
                  numberOfMonths={1}
                  disabled={{ after: new Date() }}
                />
                <Separator />
                <div className="flex items-center justify-between gap-4 p-3">
                  <span className="text-xs text-muted-foreground">
                    {t("range.hint")}
                  </span>
                  <Button
                    size="sm"
                    disabled={!draft.from || !draft.to || draft.from > draft.to}
                    onClick={() => {
                      setRange(draft);
                      setCalendarOpen(false);
                    }}
                  >
                    {t("range.apply")}
                  </Button>
                </div>
              </PopoverContent>
            </Popover>
            <Select value={model} onValueChange={setModel}>
              <SelectTrigger
                className="w-44 bg-card"
                size="sm"
                aria-label={t("model.filterAria")}
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t("model.all")}</SelectItem>
                {modelNames.map((m) => (
                  <SelectItem key={m} value={m}>
                    {m}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span className="ml-auto text-xs text-muted-foreground">
              {data?.collected_at
                ? t("meta.updatedAt", {
                    time: format(new Date(data.collected_at), "HH:mm:ss"),
                  })
                : t("meta.autoRefresh")}
            </span>
          </div>
          <div className="overflow-x-auto pb-1">
            <SegmentedControl
              ariaLabel={t("nav.aria")}
              value={nav}
              onChange={setNav}
              items={NAV_TABS.map((n) => ({
                label: t(`nav.${n}`),
                value: n,
              }))}
            />
          </div>
          {/* /usage errors belong to the Overview tab; other tabs render
              their own TabError, so the same failure never shows twice. */}
          {error && nav === "overview" && (
            <Alert variant="destructive">
              <WifiOff />
              <AlertTitle>{t("alerts.errorTitle")}</AlertTitle>
              <AlertDescription>
                {errorText(error)}.
                {data ? t("alerts.keepData") : t("alerts.checkService")}
              </AlertDescription>
            </Alert>
          )}
          {data?.configured && data.collect_err && (
            <Alert variant="destructive">
              <WifiOff />
              <AlertTitle>{t("alerts.collectTitle")}</AlertTitle>
              <AlertDescription>
                {translateBackend(data.collect_err, locale)}.
                {t("alerts.collectNote")}
              </AlertDescription>
            </Alert>
          )}
          {nav !== "overview" && (
            <>
              {nav === "cost" && (
                <CostOverview
                  range={range}
                  bucket={bucket}
                  model={model}
                  refetch={refetch}
                />
              )}
              {nav === "performance" && (
                <ModelPerformance
                  range={range}
                  bucket={bucket}
                  model={model}
                  refetch={refetch}
                />
              )}
              {nav === "compare" && (
                <UsageComparison
                  range={range}
                  model={model}
                  refetch={refetch}
                />
              )}
              {nav === "charts" && (
                <ChartsTab
                  range={range}
                  bucket={bucket}
                  model={model}
                  events={events}
                  refetch={refetch}
                />
              )}
              {nav === "requests" && (
                <RequestInspector
                  range={range}
                  model={model}
                  models={modelNames}
                  refetch={refetch}
                  live={live.revision}
                />
              )}
              {/* Pricing keeps its own sync controls; the poll tick must
                  not refetch the catalog on every revision bump. */}
              {nav === "pricing" && <PricingSettings />}
            </>
          )}
          {nav === "overview" && (
          <>
          <section
            aria-label={t("metrics.aria")}
            className="animate-in fade-in slide-in-from-bottom-2 duration-500 grid grid-cols-2 gap-3 lg:grid-cols-4"
          >
            <Metric
              label={t("metrics.total.label")}
              value={compact(total)}
              raw={total}
              format={compact}
              detail={t("metrics.total.detail", {
                input: compact(stats.input),
                output: compact(stats.output),
              })}
              help={t("metrics.total.help", { total: fmt(total) })}
              loading={loading}
            />
            <Metric
              label={t("metrics.requests.label")}
              value={fmt(stats.n)}
              raw={stats.n}
              format={(v) => fmt(Math.round(v))}
              detail={t("metrics.requests.detail", {
                count: fmt(groups(events, "model").length),
              })}
              help={t("metrics.requests.help")}
              loading={loading}
            />
            <Metric
              label={t("metrics.rate.label")}
              value={rate === null ? t("metrics.rate.none") : rate + "%"}
              raw={rate ?? undefined}
              format={(v) => v.toFixed(1) + "%"}
              detail={t("metrics.rate.detail", {
                fail: fmt(stats.fail),
                ok: fmt(stats.n - stats.fail),
              })}
              help={t("metrics.rate.help")}
              loading={loading}
            />
            <Metric
              label={t("metrics.cache.label")}
              value={
                stats.input
                  ? ((stats.cache / stats.input) * 100).toFixed(1) + "%"
                  : t("metrics.rate.none")
              }
              raw={stats.input ? (stats.cache / stats.input) * 100 : undefined}
              format={(v) => v.toFixed(1) + "%"}
              detail={t("metrics.cache.detail", {
                cache: compact(stats.cache),
              })}
              help={t("metrics.cache.help")}
              loading={loading}
            />
          </section>
          <Card className="animate-in fade-in slide-in-from-bottom-2 duration-500 [animation-delay:80ms] shadow-none">
            <CardHeader className="flex flex-wrap flex-row items-start justify-between gap-4">
              <div>
                <CardTitle className="text-base">{t("chart.title")}</CardTitle>
                <CardDescription className="mt-1.5">
                  {t("chart.description")}
                </CardDescription>
              </div>
              <SegmentedControl
                ariaLabel={t("chart.bucketAria")}
                value={bucket}
                onChange={setBucket}
                items={[
                  { label: t("chart.hour"), value: "hour" },
                  { label: t("chart.day"), value: "day" },
                  { label: t("chart.month"), value: "month" },
                ]}
              />
            </CardHeader>
            <CardContent>
              {loading ? (
                <Skeleton className="h-72 w-full" />
              ) : buckets.length ? (
                <>
                  <ChartContainer
                    className="h-[280px] w-full sm:h-[330px]"
                    config={chartConfig}
                  >
                    <AreaChart
                      accessibilityLayer
                      data={buckets}
                      margin={{ top: 12, left: 0, right: 12, bottom: 0 }}
                    >
                      <CartesianGrid vertical={false} strokeDasharray="3 4" />
                      <XAxis
                        dataKey="label"
                        axisLine={false}
                        tickLine={false}
                        minTickGap={35}
                        tickMargin={12}
                        tickFormatter={(v) =>
                          bucket === "hour"
                            ? v.slice(5)
                            : bucket === "day"
                              ? v.slice(5)
                              : v
                        }
                      />
                      <YAxis
                        tickFormatter={compact}
                        axisLine={false}
                        tickLine={false}
                        width={48}
                      />
                      <ChartTooltip
                        content={<ChartTooltipContent valueFormatter={fmt} />}
                      />
                      {["cache", "net", "output"].map((k) => (
                        <Area
                          key={k}
                          dataKey={k}
                          type="linear"
                          stackId="total"
                          stroke={`var(--color-${k})`}
                          fill={`var(--color-${k})`}
                          fillOpacity={k === "output" ? 0.8 : 0.6}
                          animationDuration={450}
                          animationEasing="ease-out"
                        />
                      ))}
                    </AreaChart>
                  </ChartContainer>
                  <div className="mt-4 flex flex-wrap justify-center gap-6 text-xs text-muted-foreground">
                    {Object.entries(chartConfig).map(([k, v]) => (
                      <span key={k} className="flex items-center gap-2">
                        <span
                          className="size-2 rounded-sm"
                          style={{ background: v.color }}
                        />
                        {v.label}
                      </span>
                    ))}
                  </div>
                </>
              ) : (
                <div className="flex h-72 flex-col items-center justify-center gap-3 text-muted-foreground">
                  <Activity className="size-8 opacity-40" />
                  <p className="text-sm">{t("chart.empty")}</p>
                  <Button variant="outline" size="sm" onClick={() => quick(30)}>
                    {t("chart.viewLast30")}
                  </Button>
                </div>
              )}
            </CardContent>
          </Card>
          <Card className="animate-in fade-in slide-in-from-bottom-2 duration-500 [animation-delay:160ms] gap-0 overflow-hidden shadow-none">
            <CardHeader className="pb-5">
              <div className="flex flex-wrap items-center justify-between gap-4">
                <div>
                  <CardTitle className="text-base">
                    {t("details.title")}
                  </CardTitle>
                  <CardDescription className="mt-1.5">
                    {t("details.description")}
                  </CardDescription>
                </div>
                <div className="relative w-full sm:w-60">
                  <Search className="absolute left-3 top-2.5 size-4 text-muted-foreground" />
                  <Input
                    className="pl-9"
                    placeholder={t("details.searchPlaceholder")}
                    aria-label={t("details.searchAria")}
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                  />
                </div>
              </div>
            </CardHeader>
            <div>
              <div className="flex flex-wrap items-center justify-between gap-3 px-6 pb-4">
                <SegmentedControl
                  ariaLabel={t("details.viewAria")}
                  value={tab}
                  onChange={setTab}
                  items={[
                    {
                      label: (
                        <>
                          <Layers className="size-3.5" />
                          {t("details.tabModel")}
                        </>
                      ),
                      value: "model",
                    },
                    {
                      label: (
                        <>
                          <KeyRound className="size-3.5" />
                          {t("details.tabKey")}
                        </>
                      ),
                      value: "key",
                    },
                    {
                      label: (
                        <>
                          <Activity className="size-3.5" />
                          {t("details.tabRequests")}
                        </>
                      ),
                      value: "requests",
                    },
                  ]}
                />
                {tab !== "requests" && (
                  <Select value={sort} onValueChange={setSort}>
                    <SelectTrigger
                      className="w-36"
                      size="sm"
                      aria-label={t("details.sortAria")}
                    >
                      <ArrowDownUp className="size-3" />
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="tokens">
                        {t("details.sortTokens")}
                      </SelectItem>
                      <SelectItem value="requests">
                        {t("details.sortRequests")}
                      </SelectItem>
                      <SelectItem value="name">
                        {t("details.sortName")}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                )}
              </div>
              <Separator />
              <Table>
                <TableHeader>
                  <TableRow>
                    {(tab === "requests"
                      ? [
                          t("table.timeModel"),
                          t("table.key"),
                          t("table.status"),
                          t("table.input"),
                          t("table.cache"),
                          t("table.output"),
                        ]
                      : [
                          t("table.modelKey"),
                          t("table.requests"),
                          t("table.success"),
                          t("table.input"),
                          t("table.cache"),
                          t("table.output"),
                          t("table.totalTokens"),
                        ]
                    ).map((s, i) => (
                      <TableHead key={i} className={i ? "text-right" : "pl-6"}>
                        {s}
                      </TableHead>
                    ))}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {shown.map((r, i) => (
                    <TableRow
                      key={tab === "requests" ? `${r.ts}-${i}` : r.name}
                      className="animate-in fade-in duration-500"
                      style={{ animationDelay: `${Math.min(i, 12) * 40}ms` }}
                    >
                      {tab === "requests" ? (
                        <>
                          <TableCell className="pl-6">
                            <div className="text-xs text-muted-foreground">
                              {format(new Date(r.ts * 1000), "MM-dd HH:mm:ss")}
                            </div>
                            <div
                              className="max-w-56 truncate font-medium"
                              title={r.model}
                            >
                              {r.model}
                            </div>
                          </TableCell>
                          <TableCell className="text-right font-mono text-xs">
                            {translateBackend(r.key, locale)}
                          </TableCell>
                          <TableCell className="text-right">
                            <Badge
                              variant={r.failed ? "destructive" : "secondary"}
                            >
                              {r.failed ? t("table.failed") : t("table.ok")}
                            </Badge>
                          </TableCell>
                        </>
                      ) : (
                        <>
                          <TableCell
                            className="max-w-64 truncate pl-6 font-medium"
                            title={translateBackend(r.name, locale)}
                          >
                            {translateBackend(r.name, locale)}
                          </TableCell>
                          <TableCell className="text-right tabular-nums">
                            {fmt(r.n)}
                          </TableCell>
                          <TableCell className="text-right tabular-nums">
                            {(((r.n - r.fail) / r.n) * 100).toFixed(1)}%
                          </TableCell>
                        </>
                      )}
                      {["input", "cache", "output"].map((k) => (
                        <TableCell
                          key={k}
                          className="text-right tabular-nums"
                          title={fmt(r[k])}
                        >
                          {compact(r[k])}
                        </TableCell>
                      ))}
                      {tab !== "requests" && (
                        <TableCell
                          className="pr-6 text-right font-medium tabular-nums"
                          title={fmt(r.input + r.output)}
                        >
                          {compact(r.input + r.output)}
                        </TableCell>
                      )}
                    </TableRow>
                  ))}
                  {!shown.length && (
                    <TableRow>
                      <TableCell
                        colSpan={7}
                        className="h-32 text-center text-muted-foreground"
                      >
                        {loading
                          ? t("details.loading")
                          : query
                            ? t("details.noMatch")
                            : t("details.empty")}
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </div>
            <Separator />
            <div className="flex items-center justify-between px-6 py-3 text-xs text-muted-foreground">
              <span>
                {t("details.footer", { count: fmt(rows.length) })}
              </span>
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
            </div>
          </Card>
          <footer className="flex flex-wrap items-center justify-between gap-2 pb-3 text-xs text-muted-foreground">
            <span className="flex items-center gap-1.5">
              <Database className="size-3" />
              {t("footer.localSqlite")} · {data?.timezone || t("footer.localTimezone")} · v
              {data?.version || "0.1.1"}
            </span>
            <span>
              {t("footer.note")}
              {data?.skipped
                ? ` · ${t("footer.skipped", { count: fmt(data.skipped) })}`
                : ""}
            </span>
          </footer>
          </>
          )}
        </main>
      </div>
    </TooltipProvider>
  );
}
createRoot(document.getElementById("root")).render(
  <I18nProvider>
    <App />
  </I18nProvider>,
);
