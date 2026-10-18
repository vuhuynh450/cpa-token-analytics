import React, { useMemo } from "react";
import {
  CartesianGrid,
  ComposedChart,
  Line,
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
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { useI18n } from "../../i18n/react.jsx";
import { pctOf } from "../../analytics";
import { TabEmpty } from "./shared.jsx";

// Latency + TTFT (ms, left axis) with failure rate (%, right axis) per bucket.
export function PerformanceTrend({ trend, loading }) {
  const { t } = useI18n();
  const chartConfig = useMemo(
    () => ({
      avg_latency: {
        label: t("perf.series.avgLatency"),
        color: "var(--chart-1)",
      },
      p95_latency: {
        label: t("perf.series.p95Latency"),
        color: "var(--chart-2)",
      },
      avg_ttft: {
        label: t("perf.series.avgTtft"),
        color: "var(--chart-3)",
      },
      failure_rate: {
        label: t("perf.series.failureRate"),
        color: "var(--destructive)",
      },
    }),
    [t],
  );
  return (
    <Card className="shadow-none">
      <CardHeader>
        <CardTitle className="text-base">{t("perf.trendTitle")}</CardTitle>
        <CardDescription className="mt-1.5">
          {t("perf.trendHint")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {loading ? (
          <Skeleton className="h-72 w-full" />
        ) : trend?.length ? (
          <>
            <ChartContainer
              className="h-[280px] w-full sm:h-[330px]"
              config={chartConfig}
            >
              <ComposedChart
                accessibilityLayer
                data={trend}
                margin={{ top: 12, left: 0, right: 0, bottom: 0 }}
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
                  yAxisId="ms"
                  axisLine={false}
                  tickLine={false}
                  width={48}
                  tickFormatter={(v) => `${v}`}
                />
                <YAxis
                  yAxisId="pct"
                  orientation="right"
                  axisLine={false}
                  tickLine={false}
                  width={40}
                  domain={[0, (max) => Math.max(0.05, max)]}
                  tickFormatter={(v) => pctOf(v, 0)}
                />
                <ChartTooltip
                  content={
                    <ChartTooltipContent
                      formatter={(value, name, item) => (
                        <div className="flex min-w-40 items-center justify-between gap-3">
                          <span className="flex items-center gap-2 text-muted-foreground">
                            <span
                              className="size-2 rounded-sm"
                              style={{ background: item.color }}
                            />
                            {chartConfig[name]?.label ?? name}
                          </span>
                          <span className="font-mono font-medium text-foreground tabular-nums">
                            {name === "failure_rate"
                              ? pctOf(value)
                              : value == null
                                ? "—"
                                : `${Number(value).toLocaleString()} ms`}
                          </span>
                        </div>
                      )}
                    />
                  }
                />
                {["avg_latency", "p95_latency", "avg_ttft"].map((k) => (
                  <Line
                    key={k}
                    yAxisId="ms"
                    dataKey={k}
                    type="linear"
                    stroke={`var(--color-${k})`}
                    strokeWidth={k === "avg_latency" ? 2 : 1.5}
                    dot={false}
                    connectNulls={false}
                    animationDuration={450}
                  />
                ))}
                <Line
                  yAxisId="pct"
                  dataKey="failure_rate"
                  type="linear"
                  stroke="var(--color-failure_rate)"
                  strokeWidth={1.5}
                  strokeDasharray="5 3"
                  dot={false}
                  connectNulls={false}
                  animationDuration={450}
                />
              </ComposedChart>
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
          <TabEmpty label={t("perf.empty")} />
        )}
      </CardContent>
    </Card>
  );
}
