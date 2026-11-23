import assert from 'node:assert/strict';
import test from 'node:test';
import { buildHistoryParams, readHistoryFilters } from '../src/history-filters.ts';
import { defaultRange, makeTimeRange, readRange, writeRange } from '../src/time-range.ts';

const emptyFilters = () => ({
  endpoint: '',
  model: '',
  backend: '',
  client_key_id: '',
  status: '',
  q: '',
  since: '',
  until: '',
});

test('history query uses the supplied anchor consistently across rerenders', () => {
  const filters = { ...emptyFilters(), q: 'hello world', status: 'error' };
  const anchor = 1_800_000_000_000;

  const first = buildHistoryParams(filters, 60_000, anchor);
  const second = buildHistoryParams(filters, 60_000, anchor);

  assert.equal(first.toString(), second.toString());
  assert.equal(first.get('since'), String(anchor - 60_000));
  assert.equal(first.get('q'), 'hello world');
  assert.equal(first.get('status'), 'error');
});

test('refreshing with a new anchor moves the generated time window', () => {
  const filters = emptyFilters();
  const rangeMs = 15 * 60_000;
  const beforeRefresh = buildHistoryParams(filters, rangeMs, 1_800_000_000_000);
  const afterRefresh = buildHistoryParams(filters, rangeMs, 1_800_000_120_000);

  assert.equal(Number(afterRefresh.get('since')) - Number(beforeRefresh.get('since')), 120_000);
});

test('custom Since overrides the global range and converts from local datetime', () => {
  const localSince = '2026-05-04T12:34';
  const filters = { ...emptyFilters(), since: localSince };
  const params = buildHistoryParams(filters, 24 * 60 * 60_000, 1_800_000_000_000);

  assert.equal(params.get('since'), String(new Date(localSince).getTime()));
});

test('history URL timestamps are read back as local datetime inputs', () => {
  const localDateTime = '2026-05-04T12:34';
  const timestamp = new Date(localDateTime).getTime();
  const filters = readHistoryFilters(`?since=${timestamp}&until=${timestamp + 60_000}&model=model-a`);

  assert.equal(filters.since, localDateTime);
  assert.equal(filters.until, '2026-05-04T12:35');
  assert.equal(filters.model, 'model-a');
});

test('invalid custom dates are omitted from the API query', () => {
  const filters = { ...emptyFilters(), since: 'not-a-date' };
  const params = buildHistoryParams(filters, 60_000, 1_800_000_000_000);

  assert.equal(params.has('since'), false);
});

test('custom date range supplies since and until when filter overrides are blank', () => {
  const filters = emptyFilters();
  const params = buildHistoryParams(filters, 120_000, 1_800_000_000_000, 1_700_000_000_000, 1_700_086_400_000);

  assert.equal(params.get('since'), '1700000000000');
  assert.equal(params.get('until'), '1700086400000');
});

test('time-range URL serialization round-trips live and fixed bounds', () => {
  const now = 1_800_000_000_000;
  const fixed = makeTimeRange(1_799_000_000_000, 1_799_500_000_000, now);
  const url = new URL('http://localhost/dash/?range=1h');
  writeRange(url, fixed);

  assert.equal(url.searchParams.has('range'), false);
  const parsed = readRange(url.search, now);
  assert.equal(parsed.from, 1_799_000_000_000);
  assert.equal(parsed.to, 1_799_500_000_000);

  const fallback = readRange('?from=invalid', now);
  const def = defaultRange(now);
  assert.equal(fallback.from, def.from);
  assert.equal(fallback.to, null);
});
