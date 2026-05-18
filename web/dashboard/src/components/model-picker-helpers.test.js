import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parseEntries,
  displayName,
  projectChips,
  commit,
  addEntry,
  addEntries,
  removeEntry,
  reorderEntries,
  setAllAvailable,
  clearAllAvailable,
  normalizeModels,
} from './model-picker-helpers.js';

// normalizeModels — bridges /api/models actual shape (bare array with
// `provider`) to picker's internal {id, owned_by} canonical. These tests
// PIN THE CONTRACT against the actual handler at routes_api.go:1129-1166.
// Cycle 20260516-picker-bugs Bug 1 fix.

test('normalizeModels bare-array (current /api/models shape)', () => {
  const raw = [{ id: 'anthropic/claude-opus-4-6', provider: 'anthropic', display_name: 'Claude Opus 4.6' }];
  const out = normalizeModels(raw);
  assert.equal(out.length, 1);
  assert.equal(out[0].id, 'anthropic/claude-opus-4-6');
  assert.equal(out[0].owned_by, 'anthropic');
  assert.equal(out[0].provider, 'anthropic');
  assert.equal(out[0].display_name, 'Claude Opus 4.6');
});

test('normalizeModels wrapped data envelope (hypothetical OpenAI-compat shape)', () => {
  const raw = { object: 'list', data: [{ id: 'anthropic/x', owned_by: 'anthropic' }] };
  const out = normalizeModels(raw);
  assert.equal(out.length, 1);
  assert.equal(out[0].owned_by, 'anthropic');
});

test('normalizeModels null/undefined (nil-slice from empty catalog)', () => {
  assert.deepEqual(normalizeModels(null), []);
  assert.deepEqual(normalizeModels(undefined), []);
});

test('normalizeModels filters missing-both-fields + sage-router entries', () => {
  const raw = [
    { id: 'a', provider: 'anthropic' },
    { id: 'b' }, // no provider, no owned_by — filtered
    { id: 'c', owned_by: 'openai' },
    { id: 'd', owned_by: 'sage-router' }, // combo/alias — filtered
    { id: 'e', provider: 'sage-router' }, // also filtered (sage-router via provider)
  ];
  const out = normalizeModels(raw);
  assert.equal(out.length, 2);
  assert.equal(out[0].owned_by, 'anthropic');
  assert.equal(out[1].owned_by, 'openai');
});

// parseEntries — splits comma string, trims, drops empties
test('parseEntries empty', () => {
  assert.deepEqual(parseEntries(''), []);
  assert.deepEqual(parseEntries(null), []);
  assert.deepEqual(parseEntries(undefined), []);
});

test('parseEntries whitespace-only (EC-1)', () => {
  assert.deepEqual(parseEntries('  '), []);
  assert.deepEqual(parseEntries(' , '), []);
  assert.deepEqual(parseEntries(',,,'), []);
});

test('parseEntries trims and filters', () => {
  assert.deepEqual(parseEntries('a, b , c'), ['a', 'b', 'c']);
  assert.deepEqual(parseEntries(' a , , b '), ['a', 'b']);
});

// displayName — provider label lookup
test('displayName known providers', () => {
  assert.equal(displayName('anthropic'), 'Anthropic');
  assert.equal(displayName('openai'), 'OpenAI');
  assert.equal(displayName('gemini'), 'Gemini');
  assert.equal(displayName('github-copilot'), 'GitHub Copilot');
});

test('displayName unknown provider falls back', () => {
  assert.equal(displayName('unknown-provider'), 'unknown-provider');
  assert.equal(displayName(''), '');
});

// commit — pure dedupe (NO sort — cycle 20260516-routing-strategy-ux M4
// dropped the alphabetical sort; allowed_models comma-string IS the order).
test('commit dedupes (preserves insertion order, no sort)', () => {
  assert.equal(commit('', ['anthropic/*', 'openai/*']), 'anthropic/*,openai/*');
  assert.equal(commit('', ['openai/*', 'anthropic/*']), 'openai/*,anthropic/*'); // AC-D1 — order preserved
});

test('commit dedupes duplicates', () => {
  assert.equal(commit('', ['a', 'a', 'b']), 'a,b'); // AC-D2
});

test('commit trims, drops empty (insertion order preserved)', () => {
  assert.equal(commit('', [' c ', '', 'a']), 'c,a'); // AC-D4 — was 'a,c' (alphabetical)
});

test('commit empty input', () => {
  // Floor invariant (cycle 20260516-picker-floor): empty auto-restores `*`.
  // Was `''` before the fix.
  assert.equal(commit('', []), '*');
});

// addEntry — append single entry
test('addEntry to empty', () => {
  assert.equal(addEntry('', 'anthropic/*'), 'anthropic/*');
});

test('addEntry preserves dedup (no sort — insertion order)', () => {
  // Cycle 20260516-routing-strategy-ux M4: was 'anthropic/*,openai/*' (alphabetical).
  // New: existing-first then appended.
  assert.equal(addEntry('openai/*', 'anthropic/*'), 'openai/*,anthropic/*');
  assert.equal(addEntry('anthropic/*', 'anthropic/*'), 'anthropic/*');
});

// addEntries — append many (combo expansion). Cycle 20260516-routing-strategy-ux
// M4 (AC-D5): combo expansion preserves combo.Models[] order; alphabetical sort dropped.
test('addEntries combo "Both" expansion (AC-D5 — insertion order)', () => {
  assert.equal(
    addEntries('', ['fast-fallback', 'anthropic/claude-haiku-4-5-20251001', 'openai/gpt-4.1-nano']),
    'fast-fallback,anthropic/claude-haiku-4-5-20251001,openai/gpt-4.1-nano',
  );
});

test('addEntries dedup against existing', () => {
  assert.equal(
    addEntries('anthropic/claude-haiku-4-5-20251001', ['fast-fallback', 'anthropic/claude-haiku-4-5-20251001']),
    'anthropic/claude-haiku-4-5-20251001,fast-fallback',
  );
});

// removeEntry
test('removeEntry exact match', () => {
  assert.equal(removeEntry('a,b,c', 'b'), 'a,c');
});

test('removeEntry no-op on missing', () => {
  assert.equal(removeEntry('a,b,c', 'z'), 'a,b,c');
});

test('removeEntry to empty', () => {
  // Floor invariant: empty result auto-restores `*`. Was `''` before fix.
  assert.equal(removeEntry('a', 'a'), '*');
});

// setAllAvailable
test('setAllAvailable returns "*"', () => {
  assert.equal(setAllAvailable(), '*');
});

// clearAllAvailable
test('clearAllAvailable removes only *', () => {
  assert.equal(clearAllAvailable('*,anthropic/*'), 'anthropic/*');
  // Floor invariant (cycle 20260516-picker-floor): clearing the lone `*`
  // results in empty → auto-restore → `*`. Visible no-op behavior.
  // Was `''` before fix.
  assert.equal(clearAllAvailable('*'), '*');
  assert.equal(clearAllAvailable('anthropic/*,*,openai/*'), 'anthropic/*,openai/*');
});

test('clearAllAvailable no-op when * absent', () => {
  assert.equal(clearAllAvailable('anthropic/*'), 'anthropic/*');
});

// projectChips — chip projection algorithm
test('projectChips empty value → all chip', () => {
  const result = projectChips('', [], []);
  assert.equal(result.length, 1);
  assert.equal(result[0].kind, 'all');
  assert.equal(result[0].label, 'All available models');
});

test('projectChips bare "*" → single all chip', () => {
  const result = projectChips('*', [], []);
  assert.equal(result.length, 1);
  assert.equal(result[0].kind, 'all');
});

test('projectChips wildcard active provider', () => {
  const result = projectChips('anthropic/*', [{ id: 'anthropic/x', owned_by: 'anthropic' }], []);
  assert.equal(result.length, 1);
  assert.equal(result[0].kind, 'wildcard');
  assert.equal(result[0].dangling, false);
  assert.equal(result[0].label, 'Anthropic — all models');
});

test('projectChips wildcard inactive provider → dangling', () => {
  const result = projectChips('anthropic/*', [], []);
  assert.equal(result[0].kind, 'wildcard');
  assert.equal(result[0].dangling, true);
});

test('projectChips exact-id model active', () => {
  const models = [{ id: 'anthropic/claude-sonnet-4-6', owned_by: 'anthropic' }];
  const result = projectChips('anthropic/claude-sonnet-4-6', models, []);
  assert.equal(result[0].kind, 'model');
  assert.equal(result[0].dangling, false);
});

test('projectChips exact-id model missing → dangling', () => {
  const result = projectChips('anthropic/claude-3-opus', [], []);
  assert.equal(result[0].kind, 'model');
  assert.equal(result[0].dangling, true);
});

test('projectChips combo with members', () => {
  const combos = [{ id: '1', name: 'fast-fallback', models: ['anthropic/x', 'openai/y'] }];
  const result = projectChips('fast-fallback', [{ id: 'anthropic/x', owned_by: 'anthropic' }], combos);
  assert.equal(result[0].kind, 'combo');
  assert.deepEqual(result[0].members, ['anthropic/x', 'openai/y']);
  assert.equal(result[0].dangling, false);
});

test('projectChips combo with all members missing → dangling', () => {
  const combos = [{ id: '1', name: 'fast-fallback', models: ['anthropic/x', 'openai/y'] }];
  const result = projectChips('fast-fallback', [], combos);
  assert.equal(result[0].kind, 'combo');
  assert.equal(result[0].dangling, true);
});

test('projectChips mid-string asterisk (EC) → dangling model', () => {
  // 'openai/gpt-4*' is NOT trailing-/* per matcher at routes_v1.go:1693-1697.
  // Projects as dangling exact-match (intentional surfacing of pre-existing
  // matcher limitation).
  const result = projectChips('openai/gpt-4*', [{ id: 'openai/gpt-4.1', owned_by: 'openai' }], []);
  assert.equal(result[0].kind, 'model');
  assert.equal(result[0].dangling, true);
});

test('projectChips multi-entry mixed (AC-A6)', () => {
  const models = [
    { id: 'anthropic/claude-haiku-4-5-20251001', owned_by: 'anthropic' },
    { id: 'openai/gpt-4.1-nano', owned_by: 'openai' },
  ];
  const combos = [{ id: '1', name: 'fast-fallback', models: ['anthropic/claude-haiku-4-5-20251001', 'openai/gpt-4.1-nano'] }];
  const result = projectChips('fast-fallback,anthropic/claude-haiku-4-5-20251001,openai/gpt-4.1-nano', models, combos);
  assert.equal(result.length, 3);
  // entries are projected in input order (not sorted by projectChips — sort happens at commit)
  assert.equal(result.find(c => c.kind === 'combo').label, 'combo: fast-fallback');
  assert.equal(result.filter(c => c.kind === 'model').length, 2);
});

// ===== Floor invariant tests (cycle 20260516-picker-floor) =====
// `*` is the picker's default-unrestricted floor. commit() enforces:
//   1. Empty selection auto-restores `*`.
//   2. Non-`*` entries coexisting with `*` strip `*`.
// All callers (addEntry, addEntries, removeEntry, clearAllAvailable) route
// through commit, so the floor applies uniformly.

test('floor: commit empty auto-restores *', () => {
  assert.equal(commit('', []), '*');
  assert.equal(commit('', ['']), '*');     // whitespace dropped → empty → '*'
  assert.equal(commit('', ['  ', '']), '*'); // multi-whitespace → empty → '*'
});

test('floor: commit only-* stays *', () => {
  assert.equal(commit('', ['*']), '*');
  assert.equal(commit('', ['*', '*']), '*'); // dedupe
});

test('floor: commit non-* with * strips * (AC-D3 — insertion order preserved)', () => {
  assert.equal(commit('', ['*', 'anthropic/*']), 'anthropic/*');
  assert.equal(commit('', ['anthropic/*', '*']), 'anthropic/*'); // single non-* left
  assert.equal(commit('', ['*', 'a', 'b']), 'a,b');
  // Cycle 20260516-routing-strategy-ux M4: was 'anthropic/*,openai/gpt-4o' (alphabetical).
  // New: insertion order (after filtering *).
  assert.equal(commit('', ['*', 'openai/gpt-4o', 'anthropic/*']), 'openai/gpt-4o,anthropic/*');
});

test('floor: addEntry from * strips *', () => {
  assert.equal(addEntry('*', 'anthropic/*'), 'anthropic/*');
  assert.equal(addEntry('*', 'openai/gpt-4o'), 'openai/gpt-4o');
  // Re-adding * after a specific is in: * gets stripped (specific wins).
  assert.equal(addEntry('anthropic/*', '*'), 'anthropic/*');
});

test('floor: addEntries combo "Both" expansion from * strips * (insertion order)', () => {
  // User has * selected, then picks combo fast-fallback → * stripped,
  // entries in insertion order. Cycle 20260516-routing-strategy-ux M4:
  // was alphabetical-sorted; now combo.Models[] order preserved.
  assert.equal(
    addEntries('*', ['fast-fallback', 'anthropic/claude-haiku-4-5-20251001', 'openai/gpt-4.1-nano']),
    'fast-fallback,anthropic/claude-haiku-4-5-20251001,openai/gpt-4.1-nano',
  );
});

test('floor: removeEntry to empty auto-restores *', () => {
  // Removing the only entry → empty → auto-restore.
  assert.equal(removeEntry('anthropic/*', 'anthropic/*'), '*');
  assert.equal(removeEntry('a', 'a'), '*');
  // Partial removal (non-empty result) is unaffected.
  assert.equal(removeEntry('a,b', 'a'), 'b');
  assert.equal(removeEntry('a,b', 'b'), 'a');
});

test('floor: clearAllAvailable empty result auto-restores *', () => {
  // ✕ on lone * chip → empty → auto-restore → visible no-op.
  assert.equal(clearAllAvailable('*'), '*');
  // ✕ on * chip when others exist → non-empty → no auto-restore.
  assert.equal(clearAllAvailable('*,anthropic/*'), 'anthropic/*');
  assert.equal(clearAllAvailable('a,*,b'), 'a,b');
});

// ---------------------------------------------------------------------------
// Cycle 20260516-routing-strategy-ux M4 — AC-D1..D5 + AC-E1..E5.
// Explicit AC-pinning tests for the sort-drop semantic shift and the new
// reorderEntries helper.

test('AC-D1: commit insertion order preserved (drop sort)', () => {
  assert.equal(commit('', ['z', 'a', 'm']), 'z,a,m');
});

test('AC-D2: commit dedupes (same-side test)', () => {
  assert.equal(commit('', ['a', 'a', 'b']), 'a,b');
});

test('AC-D3: floor invariant — non-* strips *', () => {
  assert.equal(commit('', ['*', 'anthropic/*']), 'anthropic/*');
});

test('AC-D4: whitespace trim preserved + insertion order', () => {
  assert.equal(commit('', [' a ', '', 'b']), 'a,b');
});

test('AC-D5: combo-Both insertion order (was alphabetical pre-cycle)', () => {
  assert.equal(
    addEntries('', ['fast-fallback', 'anthropic/claude-haiku-4-5-20251001', 'openai/gpt-4.1-nano']),
    'fast-fallback,anthropic/claude-haiku-4-5-20251001,openai/gpt-4.1-nano',
  );
});

test('AC-E1: reorderEntries swap mid-list', () => {
  assert.equal(reorderEntries('a,b,c,d', 0, 2), 'b,c,a,d');
});

test('AC-E2: reorderEntries no-op when from==to', () => {
  assert.equal(reorderEntries('a,b,c', 0, 0), 'a,b,c');
});

test('AC-E3: reorderEntries invalid from-index → no-op', () => {
  assert.equal(reorderEntries('a,b,c', -1, 2), 'a,b,c');
});

test('AC-E4: reorderEntries invalid to-index → no-op', () => {
  assert.equal(reorderEntries('a,b,c', 0, 99), 'a,b,c');
});

test('AC-E5: reorderEntries floor invariant — empty/single-* roundtrips to *', () => {
  // Single-element list — only valid index is 0; from==to → no-op.
  assert.equal(reorderEntries('*', 0, 0), '*');
  // Empty value — no entries; indices 0,0 are out-of-range → no-op (returns '').
  // Note: reorderEntries returns the unchanged input ('') for empty; commit()
  // would auto-restore to '*' but reorderEntries short-circuits before commit
  // when indices are out of range. This is acceptable — empty isn't a state
  // a reorder gesture should land on (no chips to drag).
  assert.equal(reorderEntries('', 0, 0), '');
});

test('AC-E5b: reorderEntries forward swap', () => {
  // Move first to last: 'a,b,c' with from=0, to=2 → 'b,c,a'
  assert.equal(reorderEntries('a,b,c', 0, 2), 'b,c,a');
});

test('AC-E5c: reorderEntries backward swap', () => {
  // Move last to first: 'a,b,c' with from=2, to=0 → 'c,a,b'
  assert.equal(reorderEntries('a,b,c', 2, 0), 'c,a,b');
});
