import React, { useMemo, useState } from "react";
import {
  Bar,
  BarChart,
  CartesianGrid,
  XAxis,
  YAxis,
} from "recharts";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { SegmentedControl } from "@/components/segmented-control";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey } from "../../analytics";
import { useI18n } from "../../i18n/react.jsx";
import { TabEmpty, TabError, useTabData } from "./shared.jsx";

const METRICS = ["tokens", "requests", "cost_usd", "avg_latency"];

const truncate = (s) => (s.length > 18 ? `${s.slice(0, 17)}…` : s);

// Top-10 models built from contract endpoints only: tokens/requests come
// from the already-loaded usage events, latency from /analytics/performance,
// cost from /analytics/cost — no extra endpoint needed.
export function ModelRanking({ range, bucket, model, events, refetch }) {
  const { t, fmt } = useI18n();
  const [metric, setMetric] = useState("tokens");
  const params = {
    start: dateKey(range.from),
    end: dateKey(range.to),
    bucket,
    model: model === "all" ? undefined : model,
  };
  const { data: perf, error: perfErr, loading: perfLoading } = useTabData(
    () => fetchAnalytics("performance", params),
    [range, bucket, model, refetch],
  );
  const { data: cost, error: costErr } = useTabData(
    () => fetchAnalytics("cost", params),
    [range, bucket, model, refetch],
  );
  const rows = useMemo(() => {
    const map = new Map();
    const row = (name) => {
      if (!map.has(name)) {
        map.set(name, { model: name, tokens: 0, requests: 0 });
      }
      return map.get(name);
    };
    for (const e of events || []) {
      const g = row(e.model);
      g.tokens += (e.input || 0) + (e.output || 0);
      g.requests += 1;
    }
    for (const m of perf?.per_model || []) {
      const g = row(m.model);
      if (typeof m.requests === "number") g.requests = m.requests;
      if (m.latency?.avg > 0) g.avg_latency = m.latency.avg;
    }
    for (const m of cost?.per_model || []) {
      const g = row(m.model);
      if (m.usd > 0) g.cost_usd = m.usd;
    }
    return [...map.values()]
      .filter(
        (r) =>
          typeof r[metric] === "number" &&
          Number.isFinite(r[metric]) &&
          r[metric] > 0,
      )
      .sort((a, b) => b[metric] - a[metric])
      .slice(0, 10);
  }, [events, perf, cost, metric]);
  const chartConfig = useMemo(
    () => ({
      value: {
        label: t(`charts.ranking.metric.${metric}`),
        color: "var(--chart-2)",
      },
    }),
    [t, metric],
  );
  const fmtValue = (v) =>
    metric === "cost_usd"
      ? `$${fmt(Math.round(v * 100) / 100)}`
      : metric === "avg_latency"
        ? `${fmt(Math.round(v))} ms`
        : fmt(v);
  const loading = perfLoading && !perf;

  return (
    <Card className="shadow-none">
      <CardHeader className="flex flex-wrap flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="text-base">
            {t("charts.ranking.title")}
          </CardTitle>
          <CardDescription className="mt-1.5">
            {t("charts.ranking.hint")}
          </CardDescription>
        </div>
        <SegmentedControl
          ariaLabel={t("charts.ranking.metricAria")}
          value={metric}
          onChange={setMetric}
          items={METRICS.map((m) => ({
            label: t(`charts.ranking.metric.${m}`),
            value: m,
          }))}
        />
      </CardHeader>
      <CardContent>
        <TabError error={perfErr || costErr} />
        {loading ? (
          <Skeleton className="h-72 w-full" />
        ) : !rows.length ? (
          <TabEmpty label={t("charts.ranking.empty")} />
        ) : (
          <ChartContainer
            className="w-full"
            style={{ height: Math.max(200, rows.length * 36) }}
            config={chartConfig}
          >
            <BarChart
              accessibilityLayer
              data={rows.map((r) => ({
                name: r.model,
                value: r[metric],
              }))}
              layout="vertical"
              margin={{ top: 4, left: 0, right: 16, bottom: 0 }}
            >
              <CartesianGrid horizontal={false} strokeDasharray="3 4" />
              <XAxis type="number" hide />
              <YAxis
                type="category"
                dataKey="name"
                axisLine={false}
                tickLine={false}
                width={130}
                tickFormatter={truncate}
              />
              <ChartTooltip
                content={
                  <ChartTooltipContent
                    formatter={(v) => (
                      <span className="font-mono font-medium text-foreground tabular-nums">
                        {fmtValue(v)}
                      </span>
                    )}
                  />
                }
              />
              <Bar
                dataKey="value"
                fill="var(--color-value)"
                radius={[0, 4, 4, 0]}
                animationDuration={450}
              />
            </BarChart>
          </ChartContainer>
        )}
      </CardContent>
    </Card>
  );
}
