import { useMemo } from 'react';
import { Info } from 'lucide-react';
import {
  ratePerSec, fmtInt, fmtMoney, fmtTokens, fmtMs,
  type TabProps,
} from '../api';
import { rollupSeries } from '../window';
import { useWindowAnalytics } from '../useWindowAnalytics';
import { Badge } from '../components/ui/badge';
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '../components/ui/empty';
import { Skeleton } from '../components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '../components/ui/tooltip';
import { TrafficAreaChart } from '../components/charts/TrafficAreaChart';
import { CostSharePie } from '../components/charts/CostSharePie';
import { InlineError, KpiCard, PanelCard } from '../components/charts/shared';

export default function Overview({ state, series, range }: TabProps) {
  const { data: detail, error: detailError, retry: retryDetail } = useWindowAnalytics(range);

  const roll = useMemo(() => rollupSeries(series), [series]);

  const rps = ratePerSec(state?.delta?.requests ?? 0, state?.delta?.d_ms || 3000);
  const errRate = roll.requests > 0 ? (roll.err / roll.requests) * 100 : 0;
  const booting = !state;

  // Exclude empty/unrouted backend bucket ("") from provider allocation:
  // requests rejected before routing never contacted an upstream provider.
  const pieRows = useMemo(
    () =>
      Object.entries(roll.backends)
        .filter(([name]) => Boolean(name))
        .map(([name, m]) => ({
          name,
          cost: m.cost,
          requests: m.requests,
        })),
    [roll],
  );

  const unpricedCostCount = detail?.summary.unknown_cost ?? 0;
  const unpricedUsageCount = detail?.summary.unknown_usage ?? 0;

  return (
    <div className="space-y-5">
      {detailError && <InlineError message={`${detailError}. Retrying automatically.`} onRetry={retryDetail} />}

      <div className="grid grid-cols-2 gap-px border border-edge-700 bg-edge-700 md:grid-cols-3 xl:grid-cols-7 [&>*]:border-0">
        {booting ? (
          Array.from({ length: 7 }).map((_, i) => <Skeleton key={i} className="h-[86px]" />)
        ) : (
          <>
            <KpiCard label="Requests" value={fmtInt(roll.requests)} sub={`${rps.toFixed(2)}/s now`} />
            <KpiCard
              label="Known spend"
              value={fmtMoney(roll.cost)}
              sub={
                unpricedCostCount > 0 ? (
                  <Tooltip>
                    <TooltipTrigger render={<span className="inline-flex cursor-help items-center gap-1 text-chart-4" />}>
                      <Info className="size-3 shrink-0" aria-hidden="true" />
                      <span>{fmtInt(unpricedCostCount)} unpriced req</span>
                    </TooltipTrigger>
                    <TooltipContent>
                      {fmtInt(unpricedCostCount)} requests in this range lack token counts or model pricing (for example, rejected or unconfigured requests) and are excluded from known spend. Lifetime known spend: {fmtMoney(state?.lifetime?.cost ?? 0)}.
                    </TooltipContent>
                  </Tooltip>
                ) : (
                  `lifetime ${fmtMoney(state?.lifetime?.cost ?? 0)}`
                )
              }
              accent="text-chart-4"
            />
            <KpiCard
              label="Tokens"
              value={fmtTokens(roll.tokIn + roll.tokOut)}
              sub={
                unpricedUsageCount > 0
                  ? `${fmtTokens(roll.tokIn)} in · ${fmtTokens(roll.tokOut)} out · ${fmtInt(unpricedUsageCount)} unmeter`
                  : `${fmtTokens(roll.tokIn)} in · ${fmtTokens(roll.tokOut)} out`
              }
            />
            <KpiCard
              label="Generated images"
              value={detail ? fmtInt(detail.summary.images) : '—'}
              sub={
                detail
                  ? detail.summary.unknown_images > 0
                    ? `${fmtInt(detail.summary.unknown_images)} without count`
                    : 'Recorded image outputs'
                  : 'Durable request detail'
              }
              accent="text-chart-5"
            />
            <KpiCard label="Error rate" value={`${errRate.toFixed(1)}%`} sub={`${fmtInt(roll.err)} errors`} accent={errRate > 0 ? 'text-destructive' : 'text-primary'} />
            <KpiCard
              label="Latency p95"
              value={detail ? fmtMs(detail.latency.p95) : <Skeleton className="h-6 w-20" />}
              sub={detail ? `p50 ${fmtMs(detail.latency.p50)} · p99 ${fmtMs(detail.latency.p99)}` : 'Loading distributions…'}
            />
            <KpiCard
              label="Average TTFT"
              value={detail ? fmtMs(detail.summary.avg_ttft_ms) : <Skeleton className="h-6 w-20" />}
              sub="Recorded streams in range"
            />
          </>
        )}
      </div>

      <PanelCard
        title="Traffic & known spend"
        description={`Requests and known cost over ${range.label}`}
        action={
          <Tooltip>
            <TooltipTrigger render={<span className="cursor-help" />}>
              <Badge variant="outline" className="gap-1">
                <Info className="size-3 text-edge-400" aria-hidden="true" />
                {range.label}
              </Badge>
            </TooltipTrigger>
            <TooltipContent>
              Traffic and spend come from gateway snapshots over {range.label}. Latency percentiles and TTFT come from request distributions over the same period.
            </TooltipContent>
          </Tooltip>
        }
      >
        <TrafficAreaChart series={series} range={range} loading={booting && !series.length} />
      </PanelCard>

      <PanelCard
        title="Provider allocation"
        description="Known spend, share, and routed request counts by upstream provider"
      >
        <CostSharePie rows={pieRows} loading={booting} />
      </PanelCard>

      {series.length === 0 && !booting && (
        <Empty className="mt-4">
          <EmptyHeader>
            <EmptyTitle>No snapshots in {range.label}</EmptyTitle>
            <EmptyDescription>Adjust the date range above to inspect gateway traffic.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}
    </div>
  );
}
