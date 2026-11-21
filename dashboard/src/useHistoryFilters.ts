import { useEffect, useMemo, useState } from 'react';
import type { RangePreset } from './api';
import { buildHistoryParams, readHistoryFilters, type HistoryFilter } from './history-filters';

export function useHistoryFilters(range: RangePreset, rangeAnchor: number) {
  const [filters, setFilters] = useState<HistoryFilter>(() => readHistoryFilters(location.search));
  const [anchor, setAnchor] = useState(Date.now);
  useEffect(() => {
    const update = () => setFilters(readHistoryFilters(location.search));
    window.addEventListener('popstate', update);
    return () => window.removeEventListener('popstate', update);
  }, []);
  useEffect(() => {
    const url = new URL(location.href);
    for (const [key, value] of Object.entries(filters)) { if (value) url.searchParams.set(key, value); else url.searchParams.delete(key); }
    history.replaceState(null, '', url);
  }, [filters]);
  const params = useMemo(
    () => buildHistoryParams(filters, range.ms, Math.max(anchor, rangeAnchor), range.from, range.to),
    [filters, range.ms, range.from, range.to, anchor, rangeAnchor],
  );
  return { filters, setFilters, params, refreshWindow: () => setAnchor(Date.now()) };
}
