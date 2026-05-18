// usage-wire tests — cycle 20260517-usage-page-filters AC13 + T9a.
//
// Pins the reactivity-asymmetry contract between Set-valued and
// primitive-valued signals:
//   - Set: identity-based change detection. Same content with NEW
//     instance fires effects (different identity).
//   - String: value-based (Object.is). Same value reassigned does NOT
//     fire. Different value does.
//
// Memory: no other signal in this codebase holds a Set; this pinpoints
// the assumption so a future refactor that switches to a non-Set
// container fails loudly here.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { signal, effect } from '@preact/signals';

test('Set signal — new Set instance triggers effect (identity-based)', () => {
  const s = signal(new Set());
  let calls = 0;
  const dispose = effect(() => { s.value; calls++; });
  assert.equal(calls, 1, 'initial effect run');

  s.value = new Set(['a']);
  assert.equal(calls, 2, 'new Set instance fires');

  // Same content, different instance — still fires (identity differs).
  s.value = new Set(['a']);
  assert.equal(calls, 3, 'new Set with same content also fires');

  dispose();
});

test('Set signal — same instance reassigned does NOT fire', () => {
  const s = signal(new Set(['a']));
  let calls = 0;
  const dispose = effect(() => { s.value; calls++; });
  assert.equal(calls, 1);

  const same = s.value;
  s.value = same;
  assert.equal(calls, 1, 'same instance reassignment is a no-op');

  dispose();
});

test('String signal — same string reassigned does NOT fire (value-based)', () => {
  // Opposite of Set semantics. AC13 names this asymmetry explicitly so
  // an agent who confuses the two reactivity models trips this test.
  const s = signal('2026-05-17');
  let calls = 0;
  const dispose = effect(() => { s.value; calls++; });
  assert.equal(calls, 1);

  s.value = '2026-05-17';
  assert.equal(calls, 1, 'same primitive value does NOT fire (signals dedupe)');

  s.value = '2026-05-18';
  assert.equal(calls, 2, 'different primitive value fires');

  dispose();
});

test('Array.from preserves Set insertion order', () => {
  // Set→Array conversion happens at fetch callsite (spec) — depends
  // on iteration order being insertion-stable.
  const s = new Set();
  s.add('c');
  s.add('a');
  s.add('b');
  assert.deepEqual(Array.from(s), ['c', 'a', 'b']);
});
