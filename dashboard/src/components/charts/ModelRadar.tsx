// Top-five model footprint: window request / known-cost / token values and
// explicitly labeled process-session error / latency counters.

import { useMemo } from 'react';
import { PolarAngleAxis, PolarGrid, Radar, RadarChart } from 'recharts';
import { fmtInt, fmtMoney, fmtMs, fmtTokens } from '../../api';
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent } from '../ui/chart';
import { ChartEmptyState, ChartSkeleton } from './shared';
import { PALETTE, shortName } from './config';

export interface ModelFootprint {
  name: string;
  requests: number;
  errors: number;
  cost: number;
  latencyMs: number;
  tokens: number;
  images: number;
}

const DIMS = [
  { key: 'requests', label: 'Requests' },
  { key: 'errors', label: 'Errors' },
  { key: 'cost', label: 'Cost' },
  { key: 'latency', label: 'Latency' },
  { key: 'tokens', label: 'Tokens' },
  { key: 'images', label: 'Known images' },
] as const;

export function ModelRadar({ models, loading }: { models: ModelFootprint[]; loading?: boolean }) {
  const { axes, ranked } = useMemo(() => {
    const ranked = [...models]
      .filter((m) => Boolean(m.name) && m.name !== '(unknown)')
      .sort((a, b) => b.requests - a.requests)
      .slice(0, 5);
    const peak = {
      requests: Math.max(...ranked.map((m) => m.requests), 1),
      errors: Math.max(...ranked.map((m) => m.errors), 1),
      cost: Math.max(...ranked.map((m) => m.cost), 1),
      latency: Math.max(...ranked.map((m) => m.latencyMs), 1),
       tokens: Math.max(...ranked.map((m) => m.tokens), 1),
       images: Math.max(...ranked.map((m) => m.images), 1),
    };
    const axes = DIMS.map((d) => {
      const row: Record<string, string | number> = { dim: d.label };
      for (const m of ranked) {
        const raw = d.key === 'latency' ? m.latencyMs : m[d.key];
        row[m.name] = (raw / peak[d.key]) * 100;
      }
      return row;
    });
    return { axes, ranked };
  }, [models]);

  if (loading) return <ChartSkeleton height={260} />;
  if (!ranked.length) {
    return <ChartEmptyState title="No models in this window" description="Per-model shares will appear here once traffic is recorded." />;
  }

  return (
    <div className="grid items-center gap-5 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
      <ChartContainer
        config={Object.fromEntries(ranked.map((m, i) => [m.name, { label: shortName(m.name), color: PALETTE.models[i % PALETTE.models.length] }]))}
        className="h-[260px] w-full"
      >
        <RadarChart data={axes} outerRadius="72%">
          <PolarGrid stroke="var(--color-edge-700)" />
          <PolarAngleAxis dataKey="dim" tick={{ fill: 'var(--color-edge-400)', fontSize: 10, fontFamily: 'var(--font-mono)' }} />
          <ChartTooltip
            content={
              <ChartTooltipContent
                formatter={(value, name) => <span className="font-mono">{Number(value).toFixed(0)} / 100 · {shortName(String(name))}</span>}
              />
            }
          />
          {ranked.map((m, i) => {
            const color = PALETTE.models[i % PALETTE.models.length];
            return <Radar key={m.name} dataKey={m.name} name={shortName(m.name)} stroke={color} fill={color} fillOpacity={0.06} strokeWidth={1.5} />;
          })}
          <ChartLegend content={<ChartLegendContent />} />
        </RadarChart>
      </ChartContainer>
      <ul className="min-w-0 divide-y divide-edge-700">
        {ranked.map((m, i) => (
          <li key={m.name} className="space-y-2 py-3 text-[11px]">
            <div className="flex min-w-0 items-center justify-between gap-2">
              <div className="flex min-w-0 items-center gap-2">
                <span className="size-2 shrink-0" style={{ background: PALETTE.models[i % PALETTE.models.length] }} />
                <span className="min-w-0 truncate font-medium text-edge-100" title={m.name}>{shortName(m.name)}</span>
              </div>
              <span className="shrink-0 font-mono text-chart-4">{fmtMoney(m.cost)}</span>
            </div>
            <dl className="grid grid-cols-2 gap-x-4 gap-y-1.5 pl-4 text-[10px] sm:grid-cols-4">
              <div><dt className="text-edge-500">Requests</dt><dd className="mt-0.5 font-mono text-edge-200">{fmtInt(m.requests)}</dd></div>
              <div><dt className="text-edge-500">Errors</dt><dd className={`mt-0.5 font-mono ${m.errors > 0 ? 'text-destructive' : 'text-edge-200'}`}>{fmtInt(m.errors)}</dd></div>
              <div><dt className="text-edge-500">Latency</dt><dd className="mt-0.5 font-mono text-edge-200">{fmtMs(m.latencyMs)}</dd></div>
              <div>
                <dt className="text-edge-500">{m.images > 0 && m.tokens === 0 ? 'Images' : 'Tokens'}</dt>
                <dd className="mt-0.5 font-mono text-edge-200">
                  {m.images > 0 && m.tokens === 0 ? `${fmtInt(m.images)} img` : fmtTokens(m.tokens)}
                </dd>
              </div>
            </dl>
          </li>
        ))}
      </ul>
      <p className="border-t border-edge-700 pt-3 text-[10px] leading-relaxed text-edge-400 lg:col-span-2">Axes normalize each metric against the top-model peak (0–100) across the top 5 configured models in the selected date range.</p>
    </div>
  );
}
