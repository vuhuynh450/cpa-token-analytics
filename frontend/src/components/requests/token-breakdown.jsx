import React from "react";
import { fmtNum } from "../../analytics.js";
import { useI18n } from "../../i18n/react.jsx";
import { tokenParts } from "../../requests.js";

// Token breakdown for the detail dialog. Components are shown as separate
// rows without summation because input already contains cache reads/writes
// and output already contains reasoning — the footnote states the overlap
// instead of double counting.
export function TokenBreakdown({ tokens }) {
  const { t, fmt } = useI18n();
  const p = tokenParts(tokens || {});
  const row = (label, value) => (
    <div
      key={label}
      className="flex items-baseline justify-between gap-4 py-1.5"
    >
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="font-medium tabular-nums" title={fmtNum(value, fmt)}>
        {fmtNum(value, fmt)}
      </span>
    </div>
  );
  return (
    <section>
      <h3 className="text-sm font-medium">{t("req.tokenBreakdown")}</h3>
      <div className="mt-2 divide-y rounded-lg border px-3">
        {row(t("req.totalTokens"), p.total)}
        {row(t("req.inputTokens"), p.input)}
        {row(t("req.baseInput"), p.baseInput)}
        {row(t("req.cacheRead"), p.cacheRead)}
        {row(t("req.cacheWrite"), p.cacheWrite)}
        {row(t("req.outputTokens"), p.output)}
        {row(t("req.reasoning"), p.reasoning)}
      </div>
      <p className="mt-2 text-xs text-muted-foreground">{t("req.tokensNote")}</p>
    </section>
  );
}
