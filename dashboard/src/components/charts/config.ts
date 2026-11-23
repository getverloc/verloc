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

export function shortName(name: string): string {
  const parts = name.split('/');
  return parts.length > 1 ? parts.slice(-2).join('/') : name;
}
