import React from "react";
import { Badge } from "@/components/ui/badge";
import { SegmentedControl } from "@/components/segmented-control";
import { usable } from "../../analytics.js";
import { useI18n } from "../../i18n/react.jsx";
import { useCurrency } from "../analytics/shared.jsx";
import { pricingStatusKey, pricingStatusVariant } from "../../requests.js";

const COMPONENTS = [
  ["input_usd", "req.costInput"],
  ["output_usd", "req.costOutput"],
  ["cache_read_usd", "req.costCacheRead"],
  ["cache_write_usd", "req.costCacheWrite"],
];

// partial_reasons carry component names ("input", "output", "cache_read",
// "cache_write") — map them to the localized component labels above.
const REASON_LABELS = {
  input: "req.costInput",
  output: "req.costOutput",
  cache_read: "req.costCacheRead",
  cache_write: "req.costCacheWrite",
};

// Cost block for the detail dialog. All figures are estimates: a null
// `cost.usd` means unpriced and renders the localized unavailable state,
// never $0. The USD/VND toggle only appears when the fx block carries an
// effective rate (useCurrency.hasVnd).
export function RequestCost({ cost, fx }) {
  const { t, fmt } = useI18n();
  const { cur, setCur, money, hasVnd, rate, stale, mode } = useCurrency(fx);
  const c = cost || {};
  const status = pricingStatusKey(c.status);
  const row = (label, usd, bold = false) => (
    <div
      key={label}
      className="flex items-baseline justify-between gap-4 py-1.5"
    >
      <span className="text-xs text-muted-foreground">{label}</span>
      {usable(usd) ? (
        <span
          className={`tabular-nums ${bold ? "font-semibold" : "font-medium"}`}
          title={typeof usd === "number" ? `$${usd.toFixed(6)}` : undefined}
        >
          {money(usd)}
        </span>
      ) : (
        <span className="text-muted-foreground">{t("req.unavailable")}</span>
      )}
    </div>
  );
  return (
    <section>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">
          {t("req.estCost")}
          {status && (
            <Badge className="ml-2" variant={pricingStatusVariant(status)}>
              {t(`cost.status.${status}`)}
            </Badge>
          )}
          {!status && (
            <Badge className="ml-2" variant="outline">
              {t("req.pricingUnavailable")}
            </Badge>
          )}
        </h3>
        {hasVnd && (
          <SegmentedControl
            ariaLabel={t("cost.currencyAria")}
            value={cur}
            onChange={setCur}
            items={[
              { label: "USD", value: "usd" },
              { label: "VND", value: "vnd" },
            ]}
          />
        )}
      </div>
      <div className="mt-2 divide-y rounded-lg border px-3">
        {row(t("req.costTotal"), c.usd, true)}
        {COMPONENTS.map(([key, label]) => row(t(label), c[key]))}
      </div>
      {!!c.partial_reasons?.length && (
        <ul className="mt-2 list-disc space-y-0.5 pl-5 text-xs text-muted-foreground">
          {c.partial_reasons.map((r, i) => (
            <li key={i}>{REASON_LABELS[r] ? t(REASON_LABELS[r]) : r}</li>
          ))}
        </ul>
      )}
      {status === "partial" && !c.partial_reasons?.length && (
        <p className="mt-2 text-xs text-muted-foreground">
          {t("req.partialEstimate")}
        </p>
      )}
      <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {c.source && (
          <span>
            {t("req.pricingSource")}: {c.source}
          </span>
        )}
        {(c.matched_model || c.matched_provider) && (
          <span>
            {t("req.matched")}:{" "}
            {c.matched_provider && c.matched_model
              ? `${c.matched_provider}/${c.matched_model}`
              : c.matched_model || c.matched_provider}
          </span>
        )}
      </div>
      {cur === "vnd" && rate > 0 && (
        <p className="mt-2 text-xs text-muted-foreground">
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
      <p className="mt-2 text-xs text-muted-foreground">
        {t("req.estBillingNote")}
      </p>
    </section>
  );
}
