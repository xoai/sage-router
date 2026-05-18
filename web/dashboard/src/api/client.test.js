// Cycle 20260517-usage-page-filters T7 — pin the buildQS contract.
// Memory anchor [ba734a82]: JS↔Go boundary must be tested against the
// ACTUAL backend behavior. The Go side uses r.URL.Query()["api_key_id"]
// which returns []string for repeated params, so the JS side must emit
// repeated params (not CSV).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildQS } from './client.js';

test('buildQS — array values emit repeated params', () => {
  const qs = buildQS({ api_key_id: ['a', 'b'] });
  // URLSearchParams preserves insertion order; assert order-independent.
  const params = new URLSearchParams(qs);
  assert.deepEqual(params.getAll('api_key_id'), ['a', 'b']);
});

test('buildQS — mixed array + scalar', () => {
  const qs = buildQS({ api_key_id: ['a', 'b'], limit: 200 });
  const params = new URLSearchParams(qs);
  assert.deepEqual(params.getAll('api_key_id'), ['a', 'b']);
  assert.equal(params.get('limit'), '200');
});

test('buildQS — single string value (back-compat)', () => {
  assert.equal(buildQS({ api_key_id: 'a' }), 'api_key_id=a');
});

test('buildQS — empty object yields empty string', () => {
  assert.equal(buildQS({}), '');
  assert.equal(buildQS(undefined), '');
  assert.equal(buildQS(null), '');
});

test('buildQS — nullish and empty-string values skipped', () => {
  // Each of these is treated as "not present". Spec AC3 + AC6.
  assert.equal(buildQS({ from: '', to: undefined, limit: null }), '');
});

test('buildQS — empty array skipped (spec AC6: empty selection = no param)', () => {
  assert.equal(buildQS({ api_key_id: [] }), '');
});

test('buildQS — numeric values stringified', () => {
  assert.equal(buildQS({ limit: 200 }), 'limit=200');
});

test('buildQS — array members stringified', () => {
  const qs = buildQS({ ids: [1, 2, 3] });
  assert.deepEqual(new URLSearchParams(qs).getAll('ids'), ['1', '2', '3']);
});
