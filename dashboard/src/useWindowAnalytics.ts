import { useEffect, useState } from 'react';
import { getAnalytics, type Analytics, type RangePreset } from './api';

// Media counts and coverage come from the durable ledger, not legacy snapshots.
export function useWindowAnalytics(range: RangePreset, backend?: string) {
  const [round, setRound] = useState(0);
  const scope = `${range.ms}:${backend || ''}:${round}`;
  const [result, setResult] = useState<{ scope: string; data: Analytics | null; error: string }>({ scope: '', data: null, error: '' });
  useEffect(() => {
    let stopped = false;
    let inFlight = false;
    const controller = new AbortController();
    const load = async () => {
      if (inFlight || stopped) return;
      inFlight = true;
      const query = new URLSearchParams();
      if (range.ms !== null) query.set('since', String(Date.now() - range.ms));
      if (backend) query.set('backend', backend);
      try {
        const response = await getAnalytics(query, controller.signal);
        if (!stopped) setResult({ scope, data: response, error: '' });
      } catch (e) {
        if (!stopped) setResult((current) => ({ scope, data: current.scope === scope ? current.data : null, error: e instanceof Error ? e.message : 'Durable usage unavailable' }));
      } finally { inFlight = false; }
    };
    void load();
    const timer = setInterval(load, 15000);
    return () => { stopped = true; controller.abort(); clearInterval(timer); };
  }, [range.ms, backend, scope]);
  return { data: result.scope === scope ? result.data : null, error: result.scope === scope ? result.error : '', retry: () => setRound((v) => v + 1) };
}
