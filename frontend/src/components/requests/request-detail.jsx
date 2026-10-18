import React, { useEffect, useRef, useState } from "react";
import { format } from "date-fns";
import { AlertTriangle } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Separator } from "@/components/ui/separator";
import { fetchRequest } from "../../services/api.js";
import { translateBackend } from "../../i18n";
import { useI18n } from "../../i18n/react.jsx";
import { useErrorText } from "../analytics/shared.jsx";
import { requestStatus, tsMillis } from "../../requests.js";
import { TokenBreakdown } from "./token-breakdown.jsx";
import { RequestPerformance } from "./request-performance.jsx";
import { RequestCost } from "./request-cost.jsx";

function Meta({ label, children }) {
  return (
    <div className="min-w-0">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-0.5 truncate text-sm font-medium">{children}</p>
    </div>
  );
}

// Fetch the detail record once per opened id; no auto-refresh inside the
// dialog (a stale list row is fine — the id is immutable). Aborted requests
// and superseded generations are ignored via the AbortController + id ref.
function useRequestDetail(id) {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const generation = useRef(0);
  useEffect(() => {
    if (!id) return undefined;
    const gen = ++generation.current;
    const ac = new AbortController();
    setData(null);
    setError("");
    setLoading(true);
    fetchRequest(id, { signal: ac.signal })
      .then((d) => {
        if (gen === generation.current) setData(d);
      })
      .catch((e) => {
        if (gen === generation.current && e.name !== "AbortError") {
          setError(e.message);
        }
      })
      .finally(() => {
        if (gen === generation.current) setLoading(false);
      });
    return () => {
      generation.current++;
      ac.abort();
    };
  }, [id]);
  return { data, error, loading };
}

export function RequestDetail({ id, open, onOpenChange }) {
  const { t, fmt, locale } = useI18n();
  const errorText = useErrorText();
  const { data, error, loading } = useRequestDetail(open ? id : null);

  const notFound = error === "http:404";
  const status = data ? requestStatus(data) : null;
  const ms = data ? tsMillis(data.ts) : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85dvh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            {t("req.detailTitle")}
            {status && (
              <Badge
                variant={
                  status === "failed"
                    ? "destructive"
                    : status === "success"
                      ? "secondary"
                      : "outline"
                }
              >
                {status === "failed"
                  ? t("req.failed")
                  : status === "success"
                    ? t("req.success")
                    : t("req.unknown")}
              </Badge>
            )}
          </DialogTitle>
          <DialogDescription className="break-all font-mono text-xs">
            {id || "—"}
          </DialogDescription>
        </DialogHeader>
        {loading && (
          <div className="space-y-3">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-32 w-full" />
            <Skeleton className="h-24 w-full" />
          </div>
        )}
        {!loading && error && (
          <Alert variant={notFound ? "default" : "destructive"}>
            <AlertTriangle />
            <AlertTitle>{t("alerts.errorTitle")}</AlertTitle>
            <AlertDescription>
              {notFound ? t("req.notFound") : errorText(error)}
            </AlertDescription>
          </Alert>
        )}
        {!loading && !error && data && (
          <div className="space-y-5">
            <section>
              <h3 className="text-sm font-medium">{t("req.general")}</h3>
              <div className="mt-2 grid grid-cols-2 gap-3 rounded-lg border p-3 sm:grid-cols-3">
                <Meta label={t("req.col.time")}>
                  {ms ? format(new Date(ms), "yyyy-MM-dd HH:mm:ss") : "—"}
                </Meta>
                <Meta label={t("req.col.model")}>{data.model || "—"}</Meta>
                <Meta label={t("req.col.provider")}>
                  {data.provider || "—"}
                </Meta>
                <Meta label={t("req.col.key")}>
                  <span className="font-mono text-xs">
                    {translateBackend(data.key, locale)}
                  </span>
                </Meta>
                <Meta label={t("req.authType")}>
                  {data.auth_type || "—"}
                </Meta>
                <Meta label={t("req.httpStatus")}>
                  {data.status_code == null
                    ? t("req.unavailable")
                    : fmt(data.status_code)}
                </Meta>
              </div>
            </section>
            {data.failed && (
              <Alert variant="destructive">
                <AlertTriangle />
                <AlertTitle>{t("req.failureTitle")}</AlertTitle>
                <AlertDescription>
                  {t("req.httpStatus")}:{" "}
                  {data.status_code == null
                    ? t("req.unavailable")
                    : fmt(data.status_code)}
                </AlertDescription>
              </Alert>
            )}
            <Separator />
            <TokenBreakdown tokens={data.tokens} />
            <Separator />
            <RequestPerformance performance={data.performance} />
            <Separator />
            <RequestCost cost={data.cost} fx={data.fx} />
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
