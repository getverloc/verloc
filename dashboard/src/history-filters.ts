export interface HistoryFilter { endpoint: string; model: string; backend: string; client_key_id: string; status: string; q: string; since: string; until: string }
export function readHistoryFilters(search: string): HistoryFilter {
  const params = new URLSearchParams(search);
  const filters = { endpoint: params.get('endpoint') || '', model: params.get('model') || '', backend: params.get('backend') || '', client_key_id: params.get('client_key_id') || '', status: params.get('status') || '', q: params.get('q') || '', since: params.get('since') || '', until: params.get('until') || '' };
  for (const key of ['since', 'until'] as const) {
    if (!/^\d+$/.test(filters[key])) continue;
    const date = new Date(Number(filters[key]));
    if (Number.isFinite(date.getTime())) filters[key] = new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  }
  return filters;
}

export function buildHistoryParams(
  filters: HistoryFilter,
  rangeMs: number | null,
  anchor: number,
  rangeFrom?: number,
  rangeTo?: number | null,
): URLSearchParams {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (!value) continue;
    if (key === 'since' || key === 'until') {
      const ts = new Date(value).getTime();
      if (Number.isFinite(ts)) query.set(key, String(ts));
    } else query.set(key, value);
  }
  if (!filters.since) {
    if (typeof rangeFrom === 'number' && rangeFrom > 0) query.set('since', String(rangeFrom));
    else if (rangeMs !== null) query.set('since', String(anchor - rangeMs));
  }
  if (!filters.until && typeof rangeTo === 'number' && rangeTo > 0) {
    query.set('until', String(rangeTo));
  }
  return query;
}
