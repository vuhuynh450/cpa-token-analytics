import React, { useMemo } from "react";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { SegmentedControl } from "@/components/segmented-control";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey, fmtNum, pricedCoverage, usable } from "../../analytics";
import { useI18n } from "../../i18n/react.jsx";
import {
  Metric,
  TabEmpty,
  TabError,
  TabLoading,
  useCurrency,
  useTabData,
} from "./shared.jsx";

const COMPONENTS = ["input", "output", "cache_read", "cache_write"];
const STATUS_VARIANT = {
  priced: "secondary",
  partial: "outline",
  ambiguous: "outline",
  unpriced: "destructive",
};

function PriceStatusBadge({ status }) {
  const { t } = useI18n();
  const s = STATUS_VARIANT[status] ? status : "unpriced";
  return (
    <Badge variant={STATUS_VARIANT[s]}>{t(`cost.status.${s}`)}</Badge>
  );
}

function CostTable({ title, rows, nameTitle, showStatus, money, fmt, emptyLabel }) {
  const { t } = useI18n();
  return (
    <Card className="gap-0 overflow-hidden shadow-none">
      <CardHeader className="pb-4">
        <CardTitle className="text-base">{title}</CardTitle>
      </CardHeader>
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">{nameTitle}</TableHead>
              <TableHead className="text-right">{t("cost.col.requests")}</TableHead>
              <TableHead className="text-right">{t("cost.col.priced")}</TableHead>
              {showStatus && (
                <TableHead className="text-right">{t("cost.col.status")}</TableHead>
              )}
              <TableHead className="pr-6 text-right">{t("cost.col.usd")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.name}>
                <TableCell
                  className="max-w-64 truncate pl-6 font-medium"
                  title={r.name}
                >
                  {r.name}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {fmt(r.requests)}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {fmtNum(r.priced, fmt)}
                </TableCell>
                {showStatus && (
                  <TableCell className="text-right">
                    <PriceStatusBadge status={r.price_status} />
                  </TableCell>
                )}
                <TableCell className="pr-6 text-right font-medium tabular-nums">
                  {money(r.usd)}
                </TableCell>
              </TableRow>
            ))}
            {!rows.length && (
              <TableRow>
                <TableCell
                  colSpan={5}
                  className="h-24 text-center text-muted-foreground"
                >
                  {emptyLabel}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
    </Card>
  );
}

export function CostOverview({ range, bucket, model, refetch }) {
  const { t, fmt, compact } = useI18n();
  const params = {
    start: dateKey(range.from),
    end: dateKey(range.to),
    bucket,
    model: model === "all" ? undefined : model,
  };
  const { data, error, loading } = useTabData(
    () => fetchAnalytics("cost", params),
    [range, bucket, model, refetch],
  );
  const { cur, setCur, money, hasVnd, rate, stale, mode } = useCurrency(data?.fx);

  // Contract B2: today/month arrive as {usd, priced_events, total_events}
  // calendar-boundary blocks; legacy *_usd scalars are the fallback.
  const periodBlock = (v, legacyUsd) => ({
    usd: v && typeof v === "object" ? v.usd : legacyUsd,
    priced: v?.priced_events,
    total: v?.total_events,
  });
  const today = periodBlock(data?.today, data?.today_usd);
  const month = periodBlock(data?.month, data?.month_usd);

  // Coverage suffix per block: " · partial · p/t priced" when partial,
  // nothing when full; "—" for an explicit 0/0; bare base when the payload
  // carries no coverage counts at all (legacy *_usd scalars).
  const coverageDetail = (base, priced, total) => {
    if (!usable(priced) || !usable(total)) return base;
    const c = pricedCoverage(priced, total);
    if (c.state === "none") return `${base} —`;
    return c.state === "partial"
      ? `${base} ${t("cost.coveragePartial", {
          priced: fmt(c.priced),
          total: fmt(c.total),
        })}`
      : base;
  };

  const totalCov = pricedCoverage(
    data?.coverage?.priced_requests,
    data?.coverage?.total_requests,
  );
  const basis = data?.estimate_basis;

  const daily = data?.daily || [];
  const chartConfig = useMemo(
    () => ({ usd: { label: t("cost.dailySeries"), color: "var(--chart-2)" } }),
    [t],
  );

  if (loading && !data) return <TabLoading />;
  return (
    <div className="space-y-6">
      <TabError error={error} />
      {!data ? (
        <TabEmpty label={t("cost.empty")} />
      ) : (
        <>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted-foreground">
              {t("cost.disclaimer")}
              {data.auth_type === "unknown" ? ` ${t("cost.authUnknown")}` : ""}
            </p>
            {hasVnd && (
              <div className="flex items-center gap-2">
                <span className="text-xs text-muted-foreground">
                  {mode === "manual" ? t("cost.manualRate") : t("cost.autoRate")}
                </span>
                <SegmentedControl
                  ariaLabel={t("cost.currencyAria")}
                  value={cur}
                  onChange={setCur}
                  items={[
                    { label: "USD", value: "usd" },
                    { label: "VND", value: "vnd" },
                  ]}
                />
              </div>
            )}
          </div>
          <section
            aria-label={t("cost.aria")}
            className="grid grid-cols-2 gap-3 lg:grid-cols-4"
          >
            <Metric
              label={t("cost.total.label")}
              value={money(data.totals?.priced_usd)}
              detail={
                totalCov.state === "none"
                  ? "—"
                  : t("cost.total.detail", {
                      pct: usable(data.coverage?.pct)
                        ? `${data.coverage.pct}%`
                        : "—",
                      priced: fmt(totalCov.priced),
                      total: fmt(totalCov.total),
                    })
              }
              help={t("cost.total.help")}
            />
            <Metric
              label={t("cost.today.label")}
              value={money(today.usd)}
              detail={coverageDetail(
                t("cost.today.detail"),
                today.priced,
                today.total,
              )}
              help={t("cost.today.help")}
            />
            <Metric
              label={t("cost.month.label")}
              value={money(month.usd)}
              detail={coverageDetail(
                t("cost.month.detail"),
                month.priced,
                month.total,
              )}
              help={t("cost.month.help")}
            />
            <Metric
              label={t("cost.unpriced.label")}
              value={fmtNum(data.totals?.unpriced_events, fmt)}
              raw={usable(data.totals?.unpriced_events) ? data.totals.unpriced_events : undefined}
              format={(v) => fmt(Math.round(v))}
              detail={t("cost.unpriced.detail", {
                priced: fmtNum(data.totals?.priced_events, fmt),
              })}
              help={t("cost.unpriced.help")}
            />
          </section>
          <Card className="shadow-none">
            <CardHeader>
              <CardTitle className="text-base">{t("cost.components")}</CardTitle>
              <CardDescription className="mt-1.5">
                {t("cost.componentsHint")}
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
                {COMPONENTS.map((k) => {
                  const v = data.totals?.by_component?.[k];
                  return (
                    <div key={k} className="rounded-lg border p-3">
                      <p className="text-xs text-muted-foreground">
                        {t(`cost.comp.${k}`)}
                      </p>
                      <p className="mt-1 text-lg font-semibold tabular-nums">
                        {v == null ? (
                          <span title={t("pricing.unavailable")}>—</span>
                        ) : (
                          money(v)
                        )}
                      </p>
                    </div>
                  );
                })}
              </div>
            </CardContent>
          </Card>
          <Card className="shadow-none">
            <CardHeader>
              <CardTitle className="text-base">{t("cost.daily")}</CardTitle>
              <CardDescription className="mt-1.5">
                {t("cost.dailyHint")}
              </CardDescription>
            </CardHeader>
            <CardContent>
              {loading ? (
                <Skeleton className="h-60 w-full" />
              ) : daily.length ? (
                <ChartContainer className="h-[240px] w-full" config={chartConfig}>
                  <AreaChart
                    accessibilityLayer
                    data={daily}
                    margin={{ top: 12, left: 0, right: 12, bottom: 0 }}
                  >
                    <CartesianGrid vertical={false} strokeDasharray="3 4" />
                    <XAxis
                      dataKey="label"
                      axisLine={false}
                      tickLine={false}
                      minTickGap={35}
                      tickMargin={12}
                      tickFormatter={(v) => String(v).slice(5)}
                    />
                    <YAxis
                      tickFormatter={compact}
                      axisLine={false}
                      tickLine={false}
                      width={48}
                    />
                    <ChartTooltip
                      content={
                        <ChartTooltipContent
                          formatter={(v) => (
                            <span className="font-mono font-medium text-foreground tabular-nums">
                              {money(v)}
                            </span>
                          )}
                        />
                      }
                    />
                    <Area
                      dataKey="usd"
                      type="linear"
                      stroke="var(--color-usd)"
                      fill="var(--color-usd)"
                      fillOpacity={0.5}
                      animationDuration={450}
                      animationEasing="ease-out"
                    />
                  </AreaChart>
                </ChartContainer>
              ) : (
                <TabEmpty label={t("cost.empty")} />
              )}
            </CardContent>
          </Card>
          <div className="grid gap-6 lg:grid-cols-2">
            <CostTable
              title={t("cost.byModel")}
              nameTitle={t("cost.col.model")}
              showStatus
              money={money}
              fmt={fmt}
              emptyLabel={t("cost.empty")}
              rows={(data.per_model || [])
                .map((r) => ({ ...r, name: r.model }))
                .sort((a, b) => (b.usd || 0) - (a.usd || 0))}
            />
            <CostTable
              title={t("cost.byKey")}
              nameTitle={t("cost.col.key")}
              money={money}
              fmt={fmt}
              emptyLabel={t("cost.empty")}
              rows={(data.per_key || [])
                .map((r) => ({ ...r, name: r.key }))
                .sort((a, b) => (b.usd || 0) - (a.usd || 0))}
            />
          </div>
          {!!data.unmatched_models?.length && (
            <p className="text-xs text-muted-foreground">
              {t("cost.unmatchedNote", {
                list: data.unmatched_models.map((m) => m.model || m).join(", "),
              })}
            </p>
          )}
          {!!data.ambiguous_models?.length && (
            <p className="text-xs text-muted-foreground">
              {t("cost.ambiguousNote", {
                list: data.ambiguous_models
                  .map((m) => (m.provider ? `${m.provider}/${m.model}` : m.model || m))
                  .join(", "),
              })}
            </p>
          )}
          {(basis || data.snapshot) && (
            <p className="text-xs text-muted-foreground">
              {basis === "latest" || (!basis && data.snapshot === "latest")
                ? t("cost.basis.latest")
                : t("cost.basis.stored")}
            </p>
          )}
          {cur === "vnd" && rate > 0 && (
            <p className="text-xs text-muted-foreground">
              {t("cost.fxNote", {
                kind: t(
                  stale
                    ? "fx.referenceRate"
                    : mode === "manual"
                      ? "cost.manualRate"
                      : "fx.autoRate",
                ),
                rate: fmt(rate),
              })}
            </p>
          )}
        </>
      )}
    </div>
  );
}
