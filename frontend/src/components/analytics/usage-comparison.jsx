import React, { useState } from "react";
import { ArrowDown, ArrowUp, Minus } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { SegmentedControl } from "@/components/segmented-control";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey, pctOf, usable, usdStr } from "../../analytics";
import { useI18n } from "../../i18n/react.jsx";
import { TabEmpty, TabError, TabLoading, useTabData } from "./shared.jsx";

const PRESETS = ["today", "7d", "30d", "month", "custom"];
const MODES = ["elapsed", "full"];

// Metric order + delta semantics (drives badge color): "good" is shown with
// the primary badge, "bad" with destructive, "neutral" stays muted — an
// increase in neutral volume metrics (tokens, requests) must not look like
// a regression.
const METRICS = [
  ["total_tokens", "neutral"],
  ["input", "neutral"],
  ["output", "neutral"],
  ["cache", "neutral"],
  ["cache_hit_rate", "good"],
  ["requests", "neutral"],
  ["succeeded", "good"],
  ["failed", "bad"],
  ["success_rate", "good"],
  ["avg_latency", "bad"],
  ["cost_usd", "bad"],
];

const RATE_KEYS = new Set(["success_rate", "cache_hit_rate"]);

// 0 is a valid value: 0% success, 0 tokens, $0.00 cost. Only
// null/undefined/non-finite renders "—".
function fmtValue(v, key, fmt) {
  if (!usable(v)) return "—";
  if (key === "cost_usd") return usdStr(v);
  if (RATE_KEYS.has(key)) return pctOf(v); // API sends 0–1 ratios
  if (key === "avg_latency") return `${fmt(Math.round(v))} ms`;
  return fmt(Math.round(v));
}

function DeltaBadge({ m, semantic }) {
  const { t } = useI18n();
  if (m.pct_change === null || m.pct_change === undefined) {
    return <Badge variant="outline">—</Badge>;
  }
  const pct = m.pct_change;
  const up = pct > 0;
  const flat = pct === 0;
  const good =
    !flat && ((up && semantic === "good") || (!up && semantic === "bad"));
  const Icon = flat ? Minus : up ? ArrowUp : ArrowDown;
  return (
    <Badge
      variant={
        flat || semantic === "neutral"
          ? "secondary"
          : good
            ? "default"
            : "destructive"
      }
      className="gap-0.5 tabular-nums"
      title={t("compare.pctTooltip")}
    >
      <Icon className="size-3" />
      {Math.abs(pct) >= 1000
        ? `${Math.round(pct)}%`
        : `${pct.toFixed(1)}%`}
    </Badge>
  );
}

export function UsageComparison({ range, model, refetch }) {
  const { t, fmt } = useI18n();
  const [preset, setPreset] = useState("7d");
  const [mode, setMode] = useState("elapsed");
  const params = {
    preset,
    mode,
    model: model === "all" ? undefined : model,
    // preset=custom compares the current control-bar range.
    ...(preset === "custom"
      ? { custom_start: dateKey(range.from), custom_end: dateKey(range.to) }
      : {}),
  };
  const { data, error, loading } = useTabData(
    () => fetchAnalytics("comparison", params),
    [preset, mode, range, model, refetch],
  );

  const periodLabel = (p) =>
    p?.label || (p?.start ? `${p.start} → ${p.end}` : "");

  return (
    <div className="space-y-6">
      <TabError error={error} />
      <div className="flex flex-wrap items-center gap-3">
        <div className="max-w-full overflow-x-auto pb-1">
          <SegmentedControl
            ariaLabel={t("compare.presetAria")}
            value={preset}
            onChange={setPreset}
            items={PRESETS.map((p) => ({
              label: t(`compare.preset.${p}`),
              value: p,
            }))}
          />
        </div>
        <SegmentedControl
          ariaLabel={t("compare.modeAria")}
          value={mode}
          onChange={setMode}
          items={MODES.map((m) => ({
            label: t(`compare.mode.${m}`),
            value: m,
          }))}
        />
      </div>
      {data && (
        <p className="text-xs text-muted-foreground">
          {t("compare.periods", {
            current: periodLabel(data.current),
            previous: periodLabel(data.previous),
          })}
        </p>
      )}
      {loading && !data ? (
        <TabLoading rows={1} />
      ) : !data ? (
        <TabEmpty label={t("compare.empty")} />
      ) : (
        <section
          aria-label={t("compare.aria")}
          className="grid grid-cols-2 gap-3 lg:grid-cols-4"
        >
          {METRICS.map(([key, semantic]) => {
            const m = data.metrics?.[key];
            if (!m) return null;
            return (
              <Card key={key} className="gap-3 shadow-none">
                <CardHeader className="flex flex-row items-center justify-between pb-0">
                  <CardDescription>{t(`compare.metric.${key}`)}</CardDescription>
                  <DeltaBadge m={m} semantic={semantic} />
                </CardHeader>
                <CardContent>
                  <div
                    className="text-2xl font-semibold tracking-tight tabular-nums"
                    title={String(m.current)}
                  >
                    {fmtValue(m.current, key, fmt)}
                  </div>
                  <p className="mt-2 text-xs text-muted-foreground">
                    {t("compare.previous", {
                      value: fmtValue(m.previous, key, fmt),
                    })}
                    {m.delta !== null && m.delta !== undefined && (
                      <>
                        {" · "}
                        {t("compare.delta", {
                          value: fmtValue(Math.abs(m.delta), key, fmt),
                          sign: m.delta > 0 ? "+" : m.delta < 0 ? "-" : "±",
                        })}
                      </>
                    )}
                  </p>
                </CardContent>
              </Card>
            );
          })}
        </section>
      )}
    </div>
  );
}
