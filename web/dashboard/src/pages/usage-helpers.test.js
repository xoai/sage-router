// usage-helpers tests — cycle 20260517-usage-page-filters T8 + T10.
//
// Date helpers: pin the ISO-Z format byte-for-byte against the server's
// `timeStr` storage. Memory [82fa8ebabd] guard.
// Gating helpers: pin AC9 / AC9b / AC9c semantics as pure functions.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  toUTCStartOfDay,
  toUTCEndOfDay,
  presetDateStrings,
  shouldShowBreakdown,
  shouldShowBudgetBar,
  singleSelectedKey,
} from './usage-helpers.js';

// ---- T8: date transformation helpers --------------------------------

// Tests assume TZ=UTC. Set via the test runner env or the script
// invoking node --test. We assert behavior under UTC because that's
// the deterministic case; a TZ shift would produce a different ISO-Z
// for the same local-day input, which is exactly what the helper is
// supposed to do.
test('toUTCStartOfDay — formats no fractional seconds (matches timeStr)', () => {
  process.env.TZ = 'UTC';
  const out = toUTCStartOfDay('2026-05-17');
  // No '.NNN' — server's timeStr drops fractions, lexicographic
  // compare requires byte-for-byte match.
  assert.equal(out, '2026-05-17T00:00:00Z');
  assert.ok(!out.includes('.'), 'must have no fractional seconds');
});

test('toUTCEndOfDay — formats end-of-day, no fractional seconds', () => {
  process.env.TZ = 'UTC';
  assert.equal(toUTCEndOfDay('2026-05-17'), '2026-05-17T23:59:59Z');
});

test('toUTCStartOfDay / toUTCEndOfDay — empty input returns empty string', () => {
  assert.equal(toUTCStartOfDay(''), '');
  assert.equal(toUTCStartOfDay(null), '');
  assert.equal(toUTCStartOfDay(undefined), '');
  assert.equal(toUTCEndOfDay(''), '');
});

test('presetDateStrings — returns YYYY-MM-DD strings with zero-padding', () => {
  // Spot-check the format: must be exactly 10 chars, dashes at 5 and 8.
  const r = presetDateStrings(7);
  assert.match(r.from, /^\d{4}-\d{2}-\d{2}$/, 'from is YYYY-MM-DD');
  assert.match(r.to, /^\d{4}-\d{2}-\d{2}$/, 'to is YYYY-MM-DD');
});

test('presetDateStrings — to is today, from is N days earlier', () => {
  // Verify the date arithmetic: 7-day preset spans 7 days in
  // milliseconds. Compare via Date parsing to avoid TZ drift.
  const r = presetDateStrings(7);
  const fromMs = new Date(r.from + 'T00:00:00').getTime();
  const toMs = new Date(r.to + 'T00:00:00').getTime();
  const diffDays = Math.round((toMs - fromMs) / (24 * 60 * 60 * 1000));
  assert.equal(diffDays, 7, 'from→to span is 7 days');
});

test('presetDateStrings — handles month/year rollover', () => {
  // Off-by-one trap: getMonth() is 0-indexed. Spot-check that the
  // 30-day preset emits a valid date string (regex match) regardless
  // of when the test runs.
  const r = presetDateStrings(30);
  assert.match(r.from, /^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/);
});

// ---- T10: gating helpers -------------------------------------------

const KEY_A = { id: 'a', name: 'A', prefix: 'sk-A', budget_monthly: 10 };
const KEY_B = { id: 'b', name: 'B', prefix: 'sk-B', budget_monthly: 0 };
const KEY_C = { id: 'c', name: 'C', prefix: 'sk-C', budget_monthly: 5 };

test('shouldShowBreakdown — 0 keys selected, 3 available → true', () => {
  assert.equal(shouldShowBreakdown(new Set(), [KEY_A, KEY_B, KEY_C]), true);
});

test('shouldShowBreakdown — 1 key selected → false (budget bar takes over)', () => {
  assert.equal(shouldShowBreakdown(new Set(['a']), [KEY_A, KEY_B, KEY_C]), false);
});

test('shouldShowBreakdown — 2 keys selected → true (filtered)', () => {
  assert.equal(shouldShowBreakdown(new Set(['a', 'b']), [KEY_A, KEY_B, KEY_C]), true);
});

test('shouldShowBreakdown — 3 of 3 selected → true', () => {
  assert.equal(shouldShowBreakdown(new Set(['a', 'b', 'c']), [KEY_A, KEY_B, KEY_C]), true);
});

test('shouldShowBreakdown — only 1 key total → always false', () => {
  // Breakdown is meaningless with a single key.
  assert.equal(shouldShowBreakdown(new Set(), [KEY_A]), false);
  assert.equal(shouldShowBreakdown(new Set(['a']), [KEY_A]), false);
});

test('shouldShowBudgetBar — exactly 1 key with budget > 0 → true', () => {
  assert.equal(shouldShowBudgetBar(new Set(['a']), [KEY_A, KEY_B, KEY_C]), true);
});

test('shouldShowBudgetBar — exactly 1 key with budget = 0 → false', () => {
  assert.equal(shouldShowBudgetBar(new Set(['b']), [KEY_A, KEY_B, KEY_C]), false);
});

test('shouldShowBudgetBar — 0 keys → false', () => {
  assert.equal(shouldShowBudgetBar(new Set(), [KEY_A, KEY_B, KEY_C]), false);
});

test('shouldShowBudgetBar — 2 keys → false (no useful aggregate)', () => {
  assert.equal(shouldShowBudgetBar(new Set(['a', 'b']), [KEY_A, KEY_B, KEY_C]), false);
});

test('singleSelectedKey — 0 selected → null', () => {
  assert.equal(singleSelectedKey(new Set(), [KEY_A]), null);
});

test('singleSelectedKey — 1 existing → returns key object', () => {
  assert.deepEqual(singleSelectedKey(new Set(['a']), [KEY_A, KEY_B]), KEY_A);
});

test('singleSelectedKey — 1 orphan id (key deleted between fetches) → null', () => {
  assert.equal(singleSelectedKey(new Set(['ghost']), [KEY_A]), null);
});

test('singleSelectedKey — 2 selected → null', () => {
  assert.equal(singleSelectedKey(new Set(['a', 'b']), [KEY_A, KEY_B]), null);
});
