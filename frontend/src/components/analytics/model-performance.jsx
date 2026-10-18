import React, { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey, pctOf, usable } from "../../analytics";
import { useI18n } from "../../i18n/react.jsx";
import { PerformanceTrend } from "./performance-trend.jsx";
import { TabError, TabLoading, useTabData } from "./shared.jsx";

// Column definitions: [i18n key, row getter, measurement flag]. Unavailable
// values (null/NaN — never plain 0) render "—" and sort to the bottom.
// The "ms" flag marks sample-based measurements (latency/ttft/speed) where
// summarizeDist never emits 0, so a received 0 still means "no sample".
const NUMERIC_COLS = [
  ["perf.col.requests", (r) => r.requests, false],
  ["perf.col.avgLatency", (r) => r.latency?.avg, true],
  ["perf.col.p50", (r) => r.latency?.p50, true],
  ["perf.col.p95", (r) => r.latency?.p95, true],
  ["perf.col.avgTtft", (r) => r.ttft?.avg, true],
  ["perf.col.successRate", (r) => r.success_rate, false],
  ["perf.col.avgOutput", (r) => r.avg_output, false],
  ["perf.col.speed", (r) => r.tok_per_sec?.avg, true],
];

export function ModelPerformance({ range, bucket, model, refetch }) {
  const { t, fmt } = useI18n();
  const params = {
    start: dateKey(range.from),
    end: dateKey(range.to),
    bucket,
    model: model === "all" ? undefined : model,
  };
  const { data, error, loading } = useTabData(
    () => fetchAnalytics("performance", params),
    [range, bucket, model, refetch],
  );
  const [sort, setSort] = useState({ col: 0, dir: -1 });

  const rows = useMemo(() => {
    const list = [...(data?.per_model || [])];
    if (sort.col === -1) {
      list.sort((a, b) => a.model.localeCompare(b.model) * sort.dir);
      return list;
    }
    const [, getter, ms] = NUMERIC_COLS[sort.col];
    list.sort((a, b) => {
      const va = usable(getter(a), ms) ? getter(a) : null;
      const vb = usable(getter(b), ms) ? getter(b) : null;
      if (va === null && vb === null)
        return a.model.localeCompare(b.model);
      if (va === null) return 1;
      if (vb === null) return -1;
      return (va - vb) * sort.dir || a.model.localeCompare(b.model);
    });
    return list;
  }, [data, sort]);

  const cell = (v, isRate, ms) => {
    if (!usable(v, ms)) return "—";
    // isRate values arrive as 0–1 ratios from the API; 0.0 → "0.0%".
    return isRate ? pctOf(v) : fmt(Math.round(v * 100) / 100);
  };

  const sortBy = (i) =>
    setSort((s) =>
      s.col === i ? { col: i, dir: -s.dir } : { col: i, dir: -1 },
    );

  if (loading && !data) return <TabLoading />;
  return (
    <div className="space-y-6">
      <TabError error={error} />
      <Card className="gap-0 overflow-hidden shadow-none">
        <CardHeader className="pb-4">
          <CardTitle className="text-base">{t("perf.title")}</CardTitle>
          <CardDescription className="mt-1.5">
            {t("perf.description")}
          </CardDescription>
        </CardHeader>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead
                  className="cursor-pointer select-none pl-6"
                  onClick={() =>
                    setSort((s) => ({
                      col: -1,
                      dir: s.col === -1 ? -s.dir : 1,
                    }))
                  }
                >
                  <span className="inline-flex items-center gap-1">
                    {t("perf.col.model")}
                    {sort.col === -1 ? (
                      sort.dir === 1 ? (
                        <ArrowUp className="size-3" />
                      ) : (
                        <ArrowDown className="size-3" />
                      )
                    ) : (
                      <ArrowUpDown className="size-3 opacity-40" />
                    )}
                  </span>
                </TableHead>
                {NUMERIC_COLS.map(([key], i) => (
                  <TableHead
                    key={key}
                    className="cursor-pointer select-none text-right"
                    onClick={() => sortBy(i)}
                  >
                    <span className="inline-flex items-center justify-end gap-1">
                      {t(key)}
                      {sort.col === i ? (
                        sort.dir === 1 ? (
                          <ArrowUp className="size-3" />
                        ) : (
                          <ArrowDown className="size-3" />
                        )
                      ) : (
                        <ArrowUpDown className="size-3 opacity-40" />
                      )}
                    </span>
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <TableRow key={r.model}>
                  <TableCell
                    className="max-w-56 truncate pl-6 font-medium"
                    title={r.model}
                  >
                    {r.model}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {cell(r.requests)}
                  </TableCell>
                  <TableCell
                    className="text-right tabular-nums"
                    title={t("perf.sampleN", { n: fmt(r.latency?.n) })}
                  >
                    {cell(r.latency?.avg, false, true)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {cell(r.latency?.p50, false, true)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {cell(r.latency?.p95, false, true)}
                  </TableCell>
                  <TableCell
                    className="text-right tabular-nums"
                    title={t("perf.sampleN", { n: fmt(r.ttft?.n) })}
                  >
                    {cell(r.ttft?.avg, false, true)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {cell(r.success_rate, true)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {cell(r.avg_output)}
                  </TableCell>
                  <TableCell
                    className="pr-6 text-right tabular-nums"
                    title={t("perf.sampleN", { n: fmt(r.tok_per_sec?.n) })}
                  >
                    {cell(r.tok_per_sec?.avg, false, true)}
                  </TableCell>
                </TableRow>
              ))}
              {!rows.length && (
                <TableRow>
                  <TableCell
                    colSpan={9}
                    className="h-32 text-center text-muted-foreground"
                  >
                    {loading ? t("details.loading") : t("perf.empty")}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
        <div className="border-t px-6 py-3 text-xs text-muted-foreground">
          {t("perf.footnote")}
        </div>
      </Card>
      <PerformanceTrend trend={data?.trend} loading={loading} />
    </div>
  );
}
