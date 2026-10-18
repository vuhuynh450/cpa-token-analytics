import React from "react";
import { fetchAnalytics } from "../../services/api.js";
import { dateKey } from "../../analytics";
import { PerformanceTrend } from "./performance-trend.jsx";
import { TokenDistribution } from "./token-distribution.jsx";
import { UsageHeatmap } from "./usage-heatmap.jsx";
import { ModelRanking } from "./model-ranking.jsx";
import { TabError, useTabData } from "./shared.jsx";

function TrendSection({ range, bucket, model, refetch }) {
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
  return (
    <>
      <TabError error={error} />
      <PerformanceTrend trend={data?.trend} loading={loading} />
    </>
  );
}

export function ChartsTab({ range, bucket, model, events, refetch }) {
  return (
    <div className="space-y-6">
      <UsageHeatmap range={range} model={model} refetch={refetch} />
      <ModelRanking
        range={range}
        bucket={bucket}
        model={model}
        events={events}
        refetch={refetch}
      />
      <TokenDistribution range={range} model={model} refetch={refetch} />
      <TrendSection
        range={range}
        bucket={bucket}
        model={model}
        refetch={refetch}
      />
    </div>
  );
}
