import React from "react";
import { usable } from "../../analytics.js";
import { useI18n } from "../../i18n/react.jsx";

// Latency / TTFT / output-speed block. Missing measurements render the
// localized "Unavailable" — never a fabricated 0 ms. The tok/s figure is a
// backend estimate and is labeled as such.
export function RequestPerformance({ performance }) {
  const { t, fmt } = useI18n();
  const p = performance || {};
  const ms = (v) =>
    usable(v) ? (
      <span className="font-medium tabular-nums" title={`${fmt(v)} ms`}>
        {fmt(Math.round(v))} ms
      </span>
    ) : (
      <span className="text-muted-foreground">{t("req.unavailable")}</span>
    );
  const speed = usable(p.output_per_sec_estimate) ? (
    <span className="font-medium tabular-nums">
      {fmt(Math.round(p.output_per_sec_estimate * 10) / 10)} tok/s
      <span className="ml-1 text-xs font-normal text-muted-foreground">
        ({t("req.estimateMark")})
      </span>
    </span>
  ) : (
    <span className="text-muted-foreground">{t("req.unavailable")}</span>
  );
  const row = (label, node) => (
    <div
      key={label}
      className="flex items-baseline justify-between gap-4 py-1.5"
    >
      <span className="text-xs text-muted-foreground">{label}</span>
      {node}
    </div>
  );
  return (
    <section>
      <h3 className="text-sm font-medium">{t("req.performance")}</h3>
      <div className="mt-2 divide-y rounded-lg border px-3">
        {row(t("req.latency"), ms(p.latency_ms))}
        {row(t("req.ttft"), ms(p.ttft_ms))}
        {row(t("req.outputSpeed"), speed)}
      </div>
    </section>
  );
}
