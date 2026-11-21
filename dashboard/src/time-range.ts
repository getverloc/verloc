// The dashboard's single reporting period. `to: null` means "up to now" and
// keeps the view live; a fixed `to` freezes the window.

export interface TimeRange {
  from: number;
  to: number | null;
  label: string;
  key: string;
  ms: number | null;
}

export const DAY_MS = 86_400_000;

export function startOfDay(ts: number): number {
  const d = new Date(ts);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

export function endOfDay(ts: number): number {
  const d = new Date(ts);
  d.setHours(23, 59, 59, 999);
  return d.getTime();
}

function fmtPoint(ts: number, end: boolean, now: number): string {
  const d = new Date(ts);
  const sameYear = d.getFullYear() === new Date(now).getFullYear();
  const date = d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', ...(sameYear ? {} : { year: 'numeric' }) });
  const boundary = end ? d.getHours() === 23 && d.getMinutes() === 59 : d.getHours() === 0 && d.getMinutes() === 0;
  return boundary ? date : `${date}, ${d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })}`;
}

/** Human label, e.g. "15 Nov – Now" or "1 Oct – 31 Oct". */
export function fmtRange(from: number, to: number | null, now = Date.now()): string {
  const fromStr = fmtPoint(from, false, now);
  if (to === null) return `${fromStr} – Now`;
  const toStr = fmtPoint(to, true, now);
  return fromStr === toStr ? fromStr : `${fromStr} – ${toStr}`;
}

export function makeTimeRange(from: number, to: number | null = null, now = Date.now()): TimeRange {
  const end = to ?? now;
  const ms = Math.max(0, end - from);
  return {
    from,
    to,
    label: fmtRange(from, to, now),
    key: `${from}:${to ?? 'now'}`,
    ms,
  };
}

export function defaultRange(now = Date.now()): TimeRange {
  // Last 7 calendar days including today, live.
  const today = startOfDay(now);
  const d = new Date(today);
  d.setDate(d.getDate() - 6);
  return makeTimeRange(d.getTime(), null, now);
}

export function readRange(search: string, now = Date.now()): TimeRange {
  const params = new URLSearchParams(search);
  const rawFrom = params.get('from') || params.get('since');
  const rawTo = params.get('to') || params.get('until');
  const from = rawFrom ? Number(rawFrom) : NaN;
  const to = rawTo ? Number(rawTo) : null;
  if (!Number.isFinite(from) || from <= 0 || from > now) return defaultRange(now);
  if (to !== null && (!Number.isFinite(to) || to <= from)) return makeTimeRange(from, null, now);
  return makeTimeRange(from, to, now);
}

export function writeRange(url: URL, range: TimeRange): void {
  url.searchParams.delete('range');
  url.searchParams.set('from', String(range.from));
  if (range.to === null) url.searchParams.delete('to');
  else url.searchParams.set('to', String(range.to));
}

export const rangeEnd = (range: TimeRange, now = Date.now()) => range.to ?? now;
export const rangeSpan = (range: TimeRange, now = Date.now()) => Math.max(0, rangeEnd(range, now) - range.from);
export const rangeKey = (range: TimeRange) => range.key;
export const isLive = (range: TimeRange) => range.to === null;

/** API query bounds for the range. */
export function rangeParams(range: TimeRange, query = new URLSearchParams()): URLSearchParams {
  query.set('since', String(range.from));
  if (range.to !== null) query.set('until', String(range.to));
  return query;
}
