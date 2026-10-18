import React, { useMemo, useState } from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { SegmentedControl } from "@/components/segmented-control";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey, heatmapCells, heatColor, usdStr } from "../../analytics";
import { useI18n } from "../../i18n/react.jsx";
import { TabEmpty, TabError, useTabData } from "./shared.jsx";

const METRICS = ["requests", "tokens", "cost_usd"];

export function UsageHeatmap({ range, model, refetch }) {
  const { t, fmt } = useI18n();
  const [metric, setMetric] = useState("requests");
  const params = {
    start: dateKey(range.from),
    end: dateKey(range.to),
    model: model === "all" ? undefined : model,
  };
  const { data, error, loading } = useTabData(
    () => fetchAnalytics("heatmap", params),
    [range, model, refetch],
  );
  const cells = useMemo(
    () => heatmapCells(data?.dow_hours, metric),
    [data, metric],
  );
  const max = data?.max?.[metric] || 0;
  const hasData = cells.some((c) => c.value > 0);

  const fmtValue = (v) =>
    metric === "cost_usd" ? usdStr(v) : fmt(v);

  return (
    <Card className="shadow-none">
      <CardHeader className="flex flex-wrap flex-row items-start justify-between gap-4">
        <div>
          <CardTitle className="text-base">{t("charts.heatmap.title")}</CardTitle>
          <CardDescription className="mt-1.5">
            {t("charts.heatmap.hint")}
          </CardDescription>
        </div>
        <SegmentedControl
          ariaLabel={t("charts.heatmap.metricAria")}
          value={metric}
          onChange={setMetric}
          items={METRICS.map((m) => ({
            label: t(`charts.heatmap.metric.${m}`),
            value: m,
          }))}
        />
      </CardHeader>
      <CardContent>
        <TabError error={error} />
        {loading && !data ? (
          <Skeleton className="h-56 w-full" />
        ) : !data || !hasData ? (
          <TabEmpty label={t("charts.heatmap.empty")} />
        ) : (
          <>
            <div className="overflow-x-auto">
              <div
                className="grid min-w-[560px] gap-[3px]"
                style={{
                  gridTemplateColumns: "2.75rem repeat(24, minmax(0, 1fr))",
                }}
              >
                <span />
                {Array.from({ length: 24 }, (_, h) => (
                  <span
                    key={h}
                    className="text-center text-[10px] text-muted-foreground"
                  >
                    {h % 3 === 0 ? h : ""}
                  </span>
                ))}
                {Array.from({ length: 7 }, (_, dow) => (
                  <React.Fragment key={dow}>
                    <span className="flex items-center text-[10px] text-muted-foreground">
                      {t(`charts.dow.${dow}`)}
                    </span>
                    {Array.from({ length: 24 }, (_, hour) => {
                      const c = cells[dow * 24 + hour];
                      return (
                        <span
                          key={hour}
                          className="h-4 rounded-[3px] sm:h-5"
                          title={`${t(`charts.dow.${dow}`)} ${String(hour).padStart(2, "0")}:00 — ${fmtValue(c.value)}`}
                          style={{
                            background: heatColor(max ? c.value / max : 0),
                          }}
                        />
                      );
                    })}
                  </React.Fragment>
                ))}
              </div>
            </div>
            <div className="mt-4 flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
              <span className="flex items-center gap-2">
                {t("charts.heatmap.low")}
                {[0, 0.25, 0.5, 0.75, 1].map((r) => (
                  <span
                    key={r}
                    className="size-3 rounded-[3px]"
                    style={{ background: heatColor(r) }}
                  />
                ))}
                {t("charts.heatmap.high")}
              </span>
              <span>
                {t("charts.heatmap.timezone", {
                  tz: data.timezone || t("footer.localTimezone"),
                })}
                {` · ${t("charts.heatmap.peak", { value: fmtValue(max) })}`}
              </span>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
