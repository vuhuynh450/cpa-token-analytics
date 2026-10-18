import React, { useMemo } from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey } from "../../analytics";
import { useI18n } from "../../i18n/react.jsx";
import { TabEmpty, TabError, useTabData } from "./shared.jsx";

// Stacked single bar per component share — components overlap conceptually
// (cache is part of input), so a donut would double-count visually.
const PARTS = [
  ["net_input", "var(--chart-2)"],
  ["cache_read", "var(--chart-1)"],
  ["cache_write", "var(--muted-foreground)"],
  ["output", "var(--chart-3)"],
];

export function TokenDistribution({ range, model, refetch }) {
  const { t, fmt, compact } = useI18n();
  const params = {
    start: dateKey(range.from),
    end: dateKey(range.to),
    model: model === "all" ? undefined : model,
  };
  const { data, error, loading } = useTabData(
    () => fetchAnalytics("overview", params),
    [range, model, refetch],
  );
  const parts = useMemo(() => {
    const totals = data?.totals || {};
    const input = totals.input || 0;
    const cacheWrite = totals.cache_write || 0;
    const output = totals.output || 0;
    // totals.cache is already cache-read only (cache_write is separate).
    const cacheRead = totals.cache || 0;
    const netInput = Math.max(0, input - cacheRead - cacheWrite);
    const values = {
      net_input: netInput,
      cache_read: cacheRead,
      cache_write: cacheWrite,
      output,
    };
    const sum = Object.values(values).reduce((a, b) => a + b, 0);
    return PARTS.map(([key, color]) => ({
      key,
      color,
      value: values[key],
      pct: sum ? (values[key] / sum) * 100 : 0,
    }));
  }, [data]);
  const total = parts.reduce((a, p) => a + p.value, 0);

  return (
    <Card className="shadow-none">
      <CardHeader>
        <CardTitle className="text-base">
          {t("charts.distribution.title")}
        </CardTitle>
        <CardDescription className="mt-1.5">
          {t("charts.distribution.hint")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <TabError error={error} />
        {loading && !data ? (
          <Skeleton className="h-40 w-full" />
        ) : !total ? (
          <TabEmpty label={t("charts.distribution.empty")} />
        ) : (
          <>
            <div
              className="flex h-6 w-full overflow-hidden rounded-md"
              role="img"
              aria-label={t("charts.distribution.title")}
            >
              {parts.map((p) =>
                p.value > 0 ? (
                  <span
                    key={p.key}
                    title={`${t(`charts.distribution.part.${p.key}`)} — ${compact(p.value)}`}
                    style={{
                      width: `${p.pct}%`,
                      background: p.color,
                      minWidth: p.pct < 1.5 ? 3 : undefined,
                    }}
                  />
                ) : null,
              )}
            </div>
            <div className="mt-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
              {parts.map((p) => (
                <div key={p.key} className="rounded-lg border p-3">
                  <p className="flex items-center gap-2 text-xs text-muted-foreground">
                    <span
                      className="size-2 rounded-sm"
                      style={{ background: p.color }}
                    />
                    {t(`charts.distribution.part.${p.key}`)}
                  </p>
                  <p className="mt-1 font-semibold tabular-nums">
                    {compact(p.value)}
                  </p>
                  <p className="text-xs text-muted-foreground tabular-nums">
                    {p.pct.toFixed(1)}% · {fmt(p.value)}
                  </p>
                </div>
              ))}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
