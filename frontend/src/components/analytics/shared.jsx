import React, { useEffect, useRef, useState } from "react";
import { Activity, CircleHelp, WifiOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { KEY_PROMPT } from "../../services/api.js";
import { fxEffectiveRate } from "../../fx.js";
import { translateBackend } from "../../i18n";
import { useI18n } from "../../i18n/react.jsx";

export function Help({ children }) {
  const { t } = useI18n();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          variant="ghost"
          size="icon"
          className="size-5 text-muted-foreground"
          aria-label={t("help.aria")}
        >
          <CircleHelp className="size-3.5" />
        </Button>
      </TooltipTrigger>
      <TooltipContent className="max-w-72">{children}</TooltipContent>
    </Tooltip>
  );
}

export function useCountUp(target, duration = 550) {
  const [value, setValue] = useState(target);
  const prev = useRef(target);
  useEffect(() => {
    const from = prev.current;
    prev.current = target;
    if (from === target) {
      setValue(target);
      return;
    }
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
      setValue(target);
      return;
    }
    let raf;
    const start = performance.now();
    const tick = (now) => {
      const progress = Math.min(1, (now - start) / duration);
      const eased = 1 - Math.pow(1 - progress, 3);
      setValue(from + (target - from) * eased);
      if (progress < 1) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [target, duration]);
  return value;
}

export function Metric({ label, value, raw, format, detail, help, loading }) {
  const animated = useCountUp(typeof raw === "number" ? raw : 0);
  const display =
    typeof raw === "number" && format ? format(animated) : value;
  return (
    <Card className="gap-3 shadow-none">
      <CardHeader className="flex flex-row items-center justify-between pb-0">
        <CardDescription>{label}</CardDescription>
        <Help>{help}</Help>
      </CardHeader>
      <CardContent>
        {loading ? (
          <Skeleton className="h-9 w-28" />
        ) : (
          <div
            className="text-2xl sm:text-3xl font-semibold tracking-tight tabular-nums"
            title={String(value)}
          >
            {display}
          </div>
        )}
        <p className="mt-2 text-xs text-muted-foreground">{detail}</p>
      </CardContent>
    </Card>
  );
}

// Localized message for the fetch error sentinels (key-prompt / http:N)
// and backend-embedded strings.
export function useErrorText() {
  const { t, locale } = useI18n();
  return (raw) => {
    if (raw === KEY_PROMPT) return t("alerts.keyPrompt");
    const m = /^http:(\d+)$/.exec(raw);
    if (m) return t("alerts.httpError", { status: m[1] });
    return translateBackend(raw, locale);
  };
}

export function TabError({ error }) {
  const { t } = useI18n();
  const errorText = useErrorText();
  if (!error) return null;
  return (
    <Alert variant="destructive">
      <WifiOff />
      <AlertTitle>{t("alerts.errorTitle")}</AlertTitle>
      <AlertDescription>
        {errorText(error)}.{t("alerts.checkService")}
      </AlertDescription>
    </Alert>
  );
}

export function TabLoading({ rows = 3 }) {
  return (
    <div className="grid gap-3">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {Array.from({ length: 4 }, (_, i) => (
          <Skeleton key={i} className="h-28 w-full" />
        ))}
      </div>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-40 w-full" />
      ))}
    </div>
  );
}

export function TabEmpty({ label }) {
  return (
    <div className="flex h-40 flex-col items-center justify-center gap-3 rounded-lg border text-muted-foreground">
      <Activity className="size-8 opacity-40" />
      <p className="text-sm">{label}</p>
    </div>
  );
}

// Fetch-on-mount + refetch when deps change. fetcher must be stable across
// renders for the same deps (wrap its params in `deps`).
export function useTabData(fetcher, deps) {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const generation = useRef(0);
  useEffect(() => {
    const id = ++generation.current;
    setLoading(true);
    fetcher()
      .then((d) => {
        if (id === generation.current) {
          setData(d);
          setError("");
        }
      })
      .catch((e) => {
        if (id === generation.current) setError(e.message);
      })
      .finally(() => {
        if (id === generation.current) setLoading(false);
      });
    return () => {
      generation.current++;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return { data, error, loading };
}

// USD display with an optional VND conversion (fx block from the pricing
// status / analytics cost payload: {rate, updated_at, mode, source, stale}).
// The toggle is rendered by the caller only when an effective rate exists.
export function useCurrency(fx) {
  const [cur, setCur] = useState("usd");
  const { intlLocale } = useI18n();
  const rate = fxEffectiveRate(fx);
  const money = (usd) => {
    if (typeof usd !== "number" || !Number.isFinite(usd)) return "—";
    if (cur === "vnd" && rate) {
      return new Intl.NumberFormat(intlLocale, {
        style: "currency",
        currency: "VND",
        maximumFractionDigits: 0,
      }).format(usd * rate);
    }
    return new Intl.NumberFormat(intlLocale, {
      style: "currency",
      currency: "USD",
      minimumFractionDigits: 2,
      maximumFractionDigits: usd > 0 && usd < 0.01 ? 4 : 2,
    }).format(usd);
  };
  return {
    cur,
    setCur,
    money,
    hasVnd: rate > 0,
    rate,
    stale: !!fx?.stale,
    mode: fx?.mode || "manual",
  };
}
