import assert from 'node:assert/strict';
import test from 'node:test';
import { buildHistoryParams, readHistoryFilters } from '../src/history-filters.ts';

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
