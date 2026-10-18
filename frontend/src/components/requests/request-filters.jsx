import React from "react";
import { ArrowDownUp, ListFilter, Search, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { SegmentedControl } from "@/components/segmented-control";
import { useI18n } from "../../i18n/react.jsx";
import {
  DEFAULT_FILTERS,
  SORT_KEYS,
  advancedFilterCount,
  hasActiveFilters,
  invalidMinKeys,
} from "../../requests.js";

function Field({ label, children }) {
  return (
    <div className="space-y-1.5">
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      {children}
    </div>
  );
}

// Filter toolbar for the Requests inspector. `filters` is controlled state
// holding raw input strings — validation (invalidMinKeys) and param mapping
// (buildRequestParams) live in src/requests.js. The shared header's date
// range and model selectors already constrain the query; the local model
// control narrows it further within this tab.
export function RequestFilters({ filters, onChange, models }) {
  const { t } = useI18n();
  const invalid = invalidMinKeys(filters);
  const advCount = advancedFilterCount(filters);
  const set = (k) => (v) => onChange({ ...filters, [k]: v });

  const thresholdInput = (key, placeholder) => (
    <Input
      type="number"
      min="0"
      step="any"
      className="h-8 w-28"
      aria-invalid={invalid.has(key)}
      placeholder={placeholder}
      value={filters[key]}
      onChange={(e) => set(key)(e.target.value)}
    />
  );

  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="relative w-full sm:w-56">
        <Search className="absolute left-3 top-2.5 size-4 text-muted-foreground" />
        <Input
          className="pl-9"
          placeholder={t("req.searchPlaceholder")}
          aria-label={t("req.search")}
          value={filters.search}
          onChange={(e) => set("search")(e.target.value)}
        />
      </div>
      {models?.length ? (
        <Select value={filters.model} onValueChange={set("model")}>
          <SelectTrigger
            className="w-44 bg-card"
            size="sm"
            aria-label={t("req.col.model")}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("model.all")}</SelectItem>
            {models.map((m) => (
              <SelectItem key={m} value={m}>
                {m}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      ) : (
        <Input
          className="w-44"
          placeholder={t("req.col.model")}
          aria-label={t("req.col.model")}
          value={filters.model === "all" ? "" : filters.model}
          onChange={(e) => set("model")(e.target.value || "all")}
        />
      )}
      <SegmentedControl
        ariaLabel={t("req.statusAria")}
        value={filters.status}
        onChange={set("status")}
        items={[
          { label: t("req.allRequests"), value: "all" },
          { label: t("req.successful"), value: "success" },
          { label: t("req.failed"), value: "failed" },
        ]}
      />
      <Select value={filters.sort} onValueChange={set("sort")}>
        <SelectTrigger
          className="w-44 bg-card"
          size="sm"
          aria-label={t("req.sortAria")}
        >
          <ArrowDownUp className="size-3" />
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {SORT_KEYS.map((s) => (
            <SelectItem key={s} value={s}>
              {t(`req.sort.${s}`)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Popover>
        <PopoverTrigger asChild>
          <Button variant="outline" size="sm" aria-label={t("req.advanced")}>
            <ListFilter />
            {t("req.advanced")}
            {advCount > 0 && (
              <Badge variant="secondary">{advCount}</Badge>
            )}
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-72 space-y-3">
          <Field label={t("req.minLatency")}>
            <div className="flex flex-wrap items-center gap-2">
              <Select value={filters.slow} onValueChange={set("slow")}>
                <SelectTrigger size="sm" className="w-36">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {["off", "s1", "s3", "s5", "s10", "custom"].map((p) => (
                    <SelectItem key={p} value={p}>
                      {t(`req.slow.${p}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {filters.slow === "custom" &&
                thresholdInput("minLatency", "ms")}
            </div>
          </Field>
          <Field label={t("req.minTokens")}>
            <div className="flex flex-wrap items-center gap-2">
              <Select value={filters.tokens} onValueChange={set("tokens")}>
                <SelectTrigger size="sm" className="w-36">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {["off", "t10k", "t50k", "t100k", "custom"].map((p) => (
                    <SelectItem key={p} value={p}>
                      {t(`req.tok.${p}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {filters.tokens === "custom" &&
                thresholdInput("minTokens", "tokens")}
            </div>
          </Field>
          <Separator />
          <Field label={t("req.minCost")}>
            <div className="flex items-center gap-2">
              {thresholdInput("minCost", "0.00")}
              <span className="text-xs text-muted-foreground">USD</span>
            </div>
          </Field>
          <Field label={t("req.pricingStatus")}>
            <Select
              value={filters.pricingStatus}
              onValueChange={set("pricingStatus")}
            >
              <SelectTrigger size="sm" className="w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">
                  {t("req.pricingStatusAll")}
                </SelectItem>
                {["priced", "partial", "unpriced", "ambiguous"].map((s) => (
                  <SelectItem key={s} value={s}>
                    {t(`cost.status.${s}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
        </PopoverContent>
      </Popover>
      {hasActiveFilters(filters) && (
        <Button
          variant="ghost"
          size="sm"
          onClick={() => onChange({ ...DEFAULT_FILTERS })}
        >
          <X />
          {t("req.clearFilters")}
        </Button>
      )}
    </div>
  );
}
