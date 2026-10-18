import React from "react";
import { format } from "date-fns";
import { ArrowDown, ArrowUp, ArrowUpDown, Eye } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { fmtNum, usable, usdStr } from "../../analytics.js";
import { translateBackend } from "../../i18n";
import { useI18n } from "../../i18n/react.jsx";
import {
  DESC_SORTS,
  columnHasSort,
  nextSort,
  requestStatus,
  tsMillis,
} from "../../requests.js";

// Cost cell: mirror useCurrency's precision rule so sub-cent costs are not
// shown as a misleading "$0.00"; the exact value sits in the title tooltip.
function costCell(v) {
  return usdStr(v, v > 0 && v < 0.01 ? 4 : 2);
}

function SortIcon({ column, sort }) {
  if (!columnHasSort(sort, column)) {
    return <ArrowUpDown className="size-3 opacity-40" />;
  }
  return DESC_SORTS.has(sort) ? (
    <ArrowDown className="size-3" />
  ) : (
    <ArrowUp className="size-3" />
  );
}

function Head({ column, sort, onSort, align = "right", children }) {
  if (!column) {
    return (
      <TableHead className={align === "right" ? "text-right" : ""}>
        {children}
      </TableHead>
    );
  }
  return (
    <TableHead
      className={`cursor-pointer select-none ${align === "right" ? "text-right" : ""}`}
      onClick={() => onSort(nextSort(sort, column))}
    >
      <span
        className={`inline-flex items-center gap-1 ${align === "right" ? "justify-end" : ""}`}
      >
        {children}
        <SortIcon column={column} sort={sort} />
      </span>
    </TableHead>
  );
}

export function RequestTable({ items, loading, sort, onSort, onView }) {
  const { t, fmt, compact, locale } = useI18n();
  // Latency/TTFT: "—" for null only; a real measured 0 is still data.
  const ms = (v) =>
    usable(v) ? (
      <span title={`${fmt(v)} ms`}>{fmtNum(v, (n) => fmt(Math.round(n)))}</span>
    ) : (
      <span className="text-muted-foreground" title={t("req.unavailable")}>
        —
      </span>
    );
  const tok = (v) => (
    <span title={fmtNum(v, fmt)}>{fmtNum(v, compact)}</span>
  );
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="pl-6">
            <button
              type="button"
              className="inline-flex cursor-pointer items-center gap-1 select-none"
              onClick={() => onSort(nextSort(sort, "time"))}
            >
              {t("req.col.time")}
              <SortIcon column="time" sort={sort} />
            </button>
          </TableHead>
          <Head column={null}>{t("req.col.model")}</Head>
          <Head column={null}>{t("req.col.provider")}</Head>
          <Head column={null}>{t("req.col.key")}</Head>
          <Head column={null}>{t("req.col.status")}</Head>
          <Head column={null}>{t("req.col.input")}</Head>
          <Head column={null}>{t("req.col.cache")}</Head>
          <Head column={null}>{t("req.col.output")}</Head>
          <Head column="total" sort={sort} onSort={onSort}>
            {t("req.col.total")}
          </Head>
          <Head column="latency" sort={sort} onSort={onSort}>
            {t("req.col.latency")}
          </Head>
          <Head column={null}>{t("req.col.ttft")}</Head>
          <Head column="cost" sort={sort} onSort={onSort}>
            {t("req.col.cost")}
          </Head>
          <TableHead className="pr-6 text-right">
            {t("req.col.action")}
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {loading &&
          !items.length &&
          Array.from({ length: 6 }, (_, i) => (
            <TableRow key={`skel-${i}`}>
              {Array.from({ length: 13 }, (_, j) => (
                <TableCell key={j} className={j === 0 ? "pl-6" : j === 12 ? "pr-6" : ""}>
                  <Skeleton className="h-5 w-full min-w-10" />
                </TableCell>
              ))}
            </TableRow>
          ))}
        {items.map((r, i) => {
          const msTs = tsMillis(r.ts);
          const status = requestStatus(r);
          return (
            <TableRow
              key={r.id ?? `${r.ts}-${i}`}
              className="animate-in fade-in cursor-pointer duration-300"
              style={{ animationDelay: `${Math.min(i, 12) * 30}ms` }}
              onClick={() => onView(r.id)}
            >
              <TableCell
                className="pl-6 text-xs text-muted-foreground tabular-nums"
                title={
                  msTs ? format(new Date(msTs), "yyyy-MM-dd HH:mm:ss") : ""
                }
              >
                {msTs ? format(new Date(msTs), "MM-dd HH:mm:ss") : "—"}
              </TableCell>
              <TableCell
                className="max-w-48 truncate font-medium"
                title={r.model || ""}
              >
                {r.model || "—"}
              </TableCell>
              <TableCell className="max-w-28 truncate" title={r.provider || ""}>
                {r.provider || "—"}
              </TableCell>
              <TableCell
                className="max-w-28 truncate font-mono text-xs"
                title={translateBackend(r.key, locale)}
              >
                {translateBackend(r.key, locale)}
              </TableCell>
              <TableCell>
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
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {tok(r.input)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {tok(r.cache)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {tok(r.output)}
              </TableCell>
              <TableCell className="text-right font-medium tabular-nums">
                {tok(r.total)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {ms(r.latency_ms)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {ms(r.ttft_ms)}
              </TableCell>
              <TableCell
                className="text-right tabular-nums"
                title={
                  r.estimated_cost_usd == null
                    ? t("req.unavailable")
                    : usdStr(r.estimated_cost_usd, 6)
                }
              >
                {r.estimated_cost_usd == null ? (
                  <span className="text-muted-foreground">—</span>
                ) : (
                  costCell(r.estimated_cost_usd)
                )}
              </TableCell>
              <TableCell className="pr-6 text-right">
                <Button
                  size="icon-xs"
                  variant="ghost"
                  aria-label={t("req.view")}
                  onClick={(e) => {
                    e.stopPropagation();
                    onView(r.id);
                  }}
                >
                  <Eye />
                </Button>
              </TableCell>
            </TableRow>
          );
        })}
        {!items.length && !loading && (
          <TableRow>
            <TableCell
              colSpan={13}
              className="h-32 text-center text-muted-foreground"
            >
              {t("req.empty")}
            </TableCell>
          </TableRow>
        )}
      </TableBody>
    </Table>
  );
}
