import assert from 'node:assert/strict';
import test from 'node:test';
import { emptyMeta, fillMetaGaps, fillModelDraftGaps, findCatalogMatch, metaFromMatch } from '../src/components/admin/shared.ts';

test('catalog lookup fills gaps without replacing manual values', () => {
  const hint = { ...emptyMeta('model'), dn: 'Catalog', pi: '2', po: '4', pc: '1', piImage: '0.01', ctx: '32000', kind: 'images', ops: ['images'], attachment: true, modalities: { input: ['image'], output: ['image'] } };
  const draft = { name: 'Manual', pricing_input: '0', context_window: '1234', tags_text: 'my-tag', attachment: false, operations: [], modalities: { input: ['text'] }, image_prices: { provider_cost: false } };
  const out = fillModelDraftGaps(draft, hint);
  assert.equal(out.name, 'Manual');
  assert.equal(out.pricing_input, '0');
  assert.equal(out.context_window, '1234');
  assert.equal(out.attachment, false);
  assert.equal(out.tags_text, 'my-tag');
  assert.deepEqual(out.operations, []);
  assert.deepEqual(out.modalities, { input: ['text'], output: ['image'] });
  assert.deepEqual(out.image_prices, { provider_cost: false });
  assert.equal(out.pricing_image, '0.01');
  assert.equal(out.pricing_output, '4');
});

test('wizard enrichment preserves local zero prices and explicit false', () => {
  const local = { ...emptyMeta('model'), pi: '0', attachment: false, kind: 'embeddings', ops: ['embeddings'] };
  const remote = { ...emptyMeta('model'), pi: '2', attachment: true };
  const out = fillMetaGaps('model', local, remote);
  assert.equal(out.pi, '0');
  assert.equal(out.attachment, false);
  assert.equal(out.kind, 'embeddings');
});

test('input-only and partial prices do not become fabricated output prices', () => {
  const model = { name: 'Embed', upstream: 'model', pricing_known: true, pricing_input: 0, pricing_output: 0, pricing_cache: 0, pricing_fields: ['input', 'cache_read'], operations: ['embeddings'] };
  const meta = metaFromMatch('model', model);
  assert.equal(meta.pi, '0');
  assert.equal(meta.pc, '0');
  assert.equal(meta.po, '');
});

test('conflicting image variants and free variants are not catalog aliases', () => {
  const base = { backend: 'a', upstream: 'model:free', image_prices: { variants: [{ size: '1024x1024', quality: '', price: '0' }] } };
  assert.equal(findCatalogMatch('model', [base], 'a'), null);
  assert.equal(findCatalogMatch('model:free', [base, { ...base, image_prices: { variants: [{ size: '1024x1024', quality: '', price: '1' }] } }], 'a'), null);
});
