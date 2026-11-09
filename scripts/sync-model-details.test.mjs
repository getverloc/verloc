import assert from 'node:assert/strict';
import test from 'node:test';
import {
  mergeModelDetails,
  parseModelDetails,
  parseOpencodeVerloc,
  parseVerlocModelMap,
} from './sync-model-details.mjs';

// Exercise the same parsers, merge and JSON serialization as the CLI, with no
// private inputs or generated seed writes.
function generate({ cost, mdPricing = {}, includeOc = true } = {}) {
  const md = parseModelDetails([
    'id\tvendor/example',
    'name\tExample',
    ...Object.entries(mdPricing).map(([key, value]) => `${key}\t${JSON.stringify(value)}`),
  ].join('\n'));
  const oc = parseOpencodeVerloc(JSON.stringify({
    provider: { verloc: { models: includeOc ? {
      example: { id: 'v/example', name: 'Example (provider)', cost },
    } : {} } },
  }));
  const entries = parseVerlocModelMap(JSON.stringify({
    model_map: { 'v/example': 'provider:vendor/example' },
  }));
  const { models } = mergeModelDetails(md, oc, entries);
  return JSON.parse(JSON.stringify(models[0]));
}

function assertPrices(model, { input, output, cache_read }, tokenPrices) {
  assert.deepEqual(model.pricing_per_million, {
    ...(input === undefined ? {} : { input }),
    ...(output === undefined ? {} : { output }),
    ...(cache_read === undefined ? {} : { cache_read }),
    currency: 'USD',
    per: '1M tokens',
  });
  assert.deepEqual(model.pricing, tokenPrices);
}

test('absent rates stay unknown, without seed-loader-promoting token strings', () => {
  const model = generate();
  assert.equal(model.pricing_known, false);
  assertPrices(model, {}, {});
});

test('partial input stays numeric and unknown; missing cache defaults to input', () => {
  const model = generate({ cost: { input: 2 } });
  assert.equal(model.pricing_known, false);
  assertPrices(model, { input: 2, cache_read: 2 }, {});
});

test('partial zero output does not invent input or cache rates', () => {
  const model = generate({ cost: { output: 0 } });
  assert.equal(model.pricing_known, false);
  assertPrices(model, { output: 0 }, {});
});

test('cache-only pricing cannot mark missing input/output known', () => {
  const model = generate({ cost: { cache_read: 0 } });
  assert.equal(model.pricing_known, false);
  assertPrices(model, { cache_read: 0 }, {});
});

for (const value of [null, '', ' ', 'invalid', 'Infinity', -1, true]) {
  test(`invalid rates remain unknown: ${JSON.stringify(value)}`, () => {
    const model = generate({ cost: { input: value, output: value, cache_read: value } });
    assert.equal(model.pricing_known, false);
    assertPrices(model, {}, {});
  });
}

test('explicit free opencode rates override paid metadata, including string zero', () => {
  const model = generate({
    cost: { input: '0', output: 0 },
    mdPricing: { input: 0.000003, output: 0.000007 },
  });
  assert.equal(model.pricing_known, true);
  assertPrices(model, { input: 0, output: 0, cache_read: 0 }, {
    prompt: '0', completion: '0', input_cache_read: '0',
  });
});

test('paid opencode rates take priority and emit per-token strings', () => {
  const model = generate({
    cost: { input: 0.3, output: 1.2, cache_read: 0.03 },
    mdPricing: { input: 0.000004, output: 0.000008, input_cache_read: 0.000002 },
  });
  assert.equal(model.pricing_known, true);
  assertPrices(model, { input: 0.3, output: 1.2, cache_read: 0.03 }, {
    prompt: String(0.3 / 1e6), completion: String(1.2 / 1e6), input_cache_read: String(0.03 / 1e6),
  });
  assert.equal(Number(model.pricing.prompt), 0.0000003);
  assert.equal(Number(model.pricing.completion), 0.0000012);
});

test('an explicit zero cache rate wins over metadata and input defaults', () => {
  const model = generate({
    cost: { input: 1, output: 2, cache_read: 0 },
    mdPricing: { input_cache_read: 0.0000005 },
  });
  assertPrices(model, { input: 1, output: 2, cache_read: 0 }, {
    prompt: '0.000001', completion: '0.000002', input_cache_read: '0',
  });
});

test('metadata fills only missing fields without replacing a known zero', () => {
  const model = generate({
    cost: { input: 0 },
    mdPricing: { input: 0.000003, output: 0.000007, input_cache_read: 0.000001 },
  });
  assert.equal(model.pricing_known, true);
  assertPrices(model, { input: 0, output: 7, cache_read: 1 }, {
    prompt: '0', completion: '0.000007', input_cache_read: '0.000001',
  });
});

for (const includeOc of [true, false]) {
  test(`metadata per-token rates normalize when opencode ${includeOc ? 'has no cost' : 'is absent'}`, () => {
    const model = generate({
      includeOc,
      mdPricing: { input: 0.0000003, output: 0.0000012 },
    });
    assert.equal(model.pricing_known, true);
    assertPrices(model, { input: 0.3, output: 1.2, cache_read: 0.3 }, {
      prompt: String(0.3 / 1e6), completion: String(1.2 / 1e6), input_cache_read: String(0.3 / 1e6),
    });
  });
}

test('partial metadata and blank metadata prices stay unknown', () => {
  const model = generate({ mdPricing: { input: '', output: 0 } });
  assert.equal(model.pricing_known, false);
  assertPrices(model, { output: 0 }, {});
});

test('metadata explicit free pricing remains known', () => {
  const model = generate({ mdPricing: { input: 0, output: '0' } });
  assert.equal(model.pricing_known, true);
  assertPrices(model, { input: 0, output: 0, cache_read: 0 }, {
    prompt: '0', completion: '0', input_cache_read: '0',
  });
});

test('metadata zero cache read wins over the cache_hit alias', () => {
  const model = generate({
    mdPricing: { input: 0.000001, output: 0.000002, input_cache_read: 0, cache_hit: 0.0000005 },
  });
  assertPrices(model, { input: 1, output: 2, cache_read: 0 }, {
    prompt: '0.000001', completion: '0.000002', input_cache_read: '0',
  });
});

test('hyper-style metadata keeps per-million units and cache_hit pricing', () => {
  const model = generate({ mdPricing: { input: 0.32664, output: 3.27, cache_hit: 0.12 } });
  assert.equal(model.pricing_known, true);
  assertPrices(model, { input: 0.32664, output: 3.27, cache_read: 0.12 }, {
    prompt: String(0.32664 / 1e6), completion: String(3.27 / 1e6), input_cache_read: String(0.12 / 1e6),
  });
});

test('tiny positive per-token rates are not rounded to free', () => {
  const model = generate({ cost: { input: 0.00001, output: 0.00002 } });
  assert.equal(model.pricing_known, true);
  assert.equal(Number(model.pricing.prompt), 0.00001 / 1e6);
  assert.equal(Number(model.pricing.completion), 0.00002 / 1e6);
  assert.ok(Number(model.pricing.prompt) > 0);
});

test('provider-name matching precedes upstream matching and preserves metadata', () => {
  const md = parseModelDetails([
    'id\tvendor/by-name\nname\tExample\ndescription\tBy name\ninput\t0\noutput\t0',
    'id\tvendor/by-id\nname\tOther\ndescription\tBy id\ninput\t0.000001\noutput\t0.000002',
  ].join('\n\n'));
  const oc = parseOpencodeVerloc(JSON.stringify({ provider: { verloc: { models: {
    alias: { id: 'v/alias', name: 'Example (provider)' },
  } } } }));
  const { models } = mergeModelDetails(md, oc, [{ verlocId: 'v/alias', upstream: 'vendor/by-id' }]);
  assert.equal(models[0].description, 'By name');
  assert.equal(models[0].owned_by, 'provider');
  assert.equal(models[0].pricing.prompt, '0');
});

for (const [metadataId, upstream] of [
  ['vendor/example-free', 'vendor/example:free'],
  ['vendor/example:free', 'vendor/example-free'],
]) {
  test(`free variant matching: ${metadataId} <- ${upstream}`, () => {
    const md = parseModelDetails(`id\t${metadataId}\nname\tFree example\ninput\t0\noutput\t0`);
    const entries = parseVerlocModelMap(JSON.stringify({ model_map: { 'v/free': `openrouter:${upstream}` } }));
    const { models } = mergeModelDetails(md, {}, entries);
    assert.equal(models[0].upstream, upstream);
    assert.equal(models[0].name, 'Free example');
    assert.equal(models[0].pricing_known, true);
    assert.equal(models[0].pricing.prompt, '0');
  });
}
