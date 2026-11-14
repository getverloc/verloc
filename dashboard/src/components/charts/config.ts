import type { SeriesPoint } from '../../api';

export const PALETTE = {
  requests: '#65e6cf',
  cost: '#eab97c',
  errors: '#f0808c',
  latency: '#72b9ff',
  ttft: '#9aabff',
  p50: '#65e6cf',
  p95: '#72b9ff',
  p99: '#9aabff',
  cache: '#9aabff',
  providers: ['#65e6cf', '#72b9ff', '#9aabff', '#eab97c', '#f0808c', '#a6d5e8', '#b8c7de'],
  models: ['#65e6cf', '#72b9ff', '#9aabff', '#eab97c', '#f0808c'],
} as const;

export const GRID_STROKE = 'var(--color-edge-700)';
export const TICK = {
  fill: 'var(--color-edge-400)',
  fontSize: 10,
  fontFamily: 'var(--font-mono)',
} as const;

// Anchor on the newest sample, so saved and stale snapshot windows still chart.
export const CHART_RANGES = [
  { value: '7d', label: '7 days', days: 7 },
  { value: '30d', label: '30 days', days: 30 },
  { value: '90d', label: '90 days', days: 90 },
] as const;
export type ChartRangeKey = (typeof CHART_RANGES)[number]['value'];

export function filterSeriesByDays(series: SeriesPoint[], days: number): SeriesPoint[] {
  if (!series.length) return series;
  const newest = series.reduce((m, p) => Math.max(m, p.ts), 0);
  return series.filter((p) => p.ts >= newest - days * 86_400_000);
}

export function shortName(name: string): string {
  const parts = name.split('/');
  return parts.length > 1 ? parts.slice(-2).join('/') : name;
}
