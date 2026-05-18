// connect-helpers tests — cycle 20260518-connect-page-guide T1 + provider-wildcard fix.
// 32 cases: 15 filterAllowed + 5 pickInitialSelection + 12 getConfigFile.
// (filterAllowed grew from 7 → 13 → 15: provider/* shape per
// model-picker.jsx:22 contract + backend-parity bare-prefix branch per
// routes_v1.go:2257 + empty-string-provider defensive symmetry.)

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  filterAllowed,
  pickInitialSelection,
  getConfigFile,
} from './connect-helpers.js';

// ---- filterAllowed -----------------------------------------------------

test('filterAllowed — wildcard returns all items', () => {
  assert.deepEqual(
    filterAllowed([{ id: 'a' }, { id: 'b' }], ['*'], 'id'),
    [{ id: 'a' }, { id: 'b' }],
  );
});

test('filterAllowed — empty allowed list returns empty', () => {
  assert.deepEqual(filterAllowed([{ id: 'a' }, { id: 'b' }], [], 'id'), []);
});

test('filterAllowed — single match by id', () => {
  assert.deepEqual(filterAllowed([{ id: 'a' }, { id: 'b' }], ['a'], 'id'), [{ id: 'a' }]);
});

test('filterAllowed — match by alternate field (name)', () => {
  assert.deepEqual(
    filterAllowed([{ name: 'x' }, { name: 'y' }], ['x'], 'name'),
    [{ name: 'x' }],
  );
});

test('filterAllowed — multiple matches preserved in input order', () => {
  assert.deepEqual(
    filterAllowed([{ id: 'a' }, { id: 'b' }, { id: 'c' }], ['a', 'c'], 'id'),
    [{ id: 'a' }, { id: 'c' }],
  );
});

test('filterAllowed — empty items input returns empty', () => {
  assert.deepEqual(filterAllowed([], ['a'], 'id'), []);
});

test('filterAllowed — wildcard wins even when paired with specific entries', () => {
  assert.deepEqual(filterAllowed([{ id: 'a' }], ['*', 'a'], 'id'), [{ id: 'a' }]);
});

// ---- filterAllowed: provider/* wildcard pattern -----------------------
// Mirrors backend matchModelPattern (routes_v1.go:2249-2265) semantics.
// Catalog model objects carry both .id (= provider/slug) and .provider.

test('filterAllowed — provider/* matches all items with that provider', () => {
  const items = [
    { id: 'anthropic/claude-haiku', provider: 'anthropic' },
    { id: 'anthropic/claude-sonnet', provider: 'anthropic' },
    { id: 'openai/gpt-5', provider: 'openai' },
  ];
  assert.deepEqual(
    filterAllowed(items, ['anthropic/*'], 'id'),
    [{ id: 'anthropic/claude-haiku', provider: 'anthropic' },
     { id: 'anthropic/claude-sonnet', provider: 'anthropic' }],
  );
});

test('filterAllowed — multiple provider wildcards union', () => {
  const items = [
    { id: 'anthropic/claude-haiku', provider: 'anthropic' },
    { id: 'openai/gpt-5', provider: 'openai' },
    { id: 'google/gemini-pro', provider: 'google' },
  ];
  assert.deepEqual(
    filterAllowed(items, ['anthropic/*', 'openai/*'], 'id'),
    [{ id: 'anthropic/claude-haiku', provider: 'anthropic' },
     { id: 'openai/gpt-5', provider: 'openai' }],
  );
});

test('filterAllowed — provider/* mixed with exact id', () => {
  const items = [
    { id: 'anthropic/claude-haiku', provider: 'anthropic' },
    { id: 'openai/gpt-5', provider: 'openai' },
    { id: 'openai/gpt-5-mini', provider: 'openai' },
  ];
  assert.deepEqual(
    filterAllowed(items, ['anthropic/*', 'openai/gpt-5'], 'id'),
    [{ id: 'anthropic/claude-haiku', provider: 'anthropic' },
     { id: 'openai/gpt-5', provider: 'openai' }],
  );
});

test('filterAllowed — provider/* with no matching items returns empty for that branch', () => {
  // User added cohere/* but catalog has no cohere models — none match.
  const items = [
    { id: 'anthropic/claude-haiku', provider: 'anthropic' },
    { id: 'openai/gpt-5', provider: 'openai' },
  ];
  assert.deepEqual(filterAllowed(items, ['cohere/*'], 'id'), []);
});

test('filterAllowed — combos (no provider field) unaffected by provider/* entries', () => {
  // Combos have .name but no .provider; the provider-wildcard branch
  // is defensively skipped (item.provider is undefined). Exact name
  // match still works.
  const combos = [
    { name: 'fast-fallback' },
    { name: 'cheap-fallback' },
  ];
  assert.deepEqual(
    filterAllowed(combos, ['anthropic/*', 'fast-fallback'], 'name'),
    [{ name: 'fast-fallback' }],
  );
});

test('filterAllowed — model entry missing provider field falls through cleanly', () => {
  // Defensive: a model item without a provider field should not match
  // any provider/* wildcard (no false positive from undefined + '/*').
  const items = [{ id: 'anthropic/claude-haiku' }]; // no .provider
  assert.deepEqual(filterAllowed(items, ['anthropic/*'], 'id'), []);
});

test('filterAllowed — empty-string provider field does NOT match (defensive symmetry)', () => {
  // Mirror of the "missing provider" defensive case for empty string.
  // `'' + '/*' === '/*'` could false-positive if an allowed entry is
  // literally '/*' — pin the truthiness guard.
  const items = [{ id: 'x', provider: '' }];
  assert.deepEqual(filterAllowed(items, ['/*'], 'id'), []);
});

test('filterAllowed — backend-parity: bare prefix id matches provider/* wildcard', () => {
  // routes_v1.go:2257 `model == prefix` branch. Unreachable from the
  // current dashboard catalog (always provider/slug shape) but pinned
  // for parity. A model with id literally equal to a pattern's bare
  // prefix MUST match — regardless of whether the .provider field is
  // set or what it points to.
  const items = [
    { id: 'anthropic', provider: 'anthropic' },
    { id: 'openai', provider: 'something-else' }, // mismatched provider field
    { id: 'gemini' }, // no provider field at all
  ];
  assert.deepEqual(
    filterAllowed(items, ['anthropic/*', 'openai/*', 'gemini/*'], 'id'),
    items, // all three match via bare-prefix branch
  );
});

// ---- pickInitialSelection ----------------------------------------------

test('pickInitialSelection — current value still in filtered → keep it', () => {
  assert.equal(pickInitialSelection('a', [{ id: 'a' }, { id: 'b' }], 'id'), 'a');
});

test('pickInitialSelection — current value not in filtered → pick first', () => {
  assert.equal(pickInitialSelection('z', [{ id: 'a' }, { id: 'b' }], 'id'), 'a');
});

test('pickInitialSelection — filtered empty → return empty string', () => {
  assert.equal(pickInitialSelection('a', [], 'id'), '');
});

test('pickInitialSelection — empty current + non-empty filtered → first', () => {
  assert.equal(pickInitialSelection('', [{ id: 'a' }, { id: 'b' }], 'id'), 'a');
});

test('pickInitialSelection — match by alternate field (name)', () => {
  assert.equal(pickInitialSelection('a', [{ name: 'a' }], 'name'), 'a');
});

// ---- getConfigFile -----------------------------------------------------

test('getConfigFile — claude-code returns shell rc path', () => {
  const path = getConfigFile('claude-code');
  assert.ok(path && (path.includes('.zshrc') || path.includes('.bashrc')),
    `expected shell rc path, got: ${path}`);
});

test('getConfigFile — codex returns shell rc path', () => {
  const path = getConfigFile('codex');
  assert.ok(path && (path.includes('.zshrc') || path.includes('.bashrc')),
    `expected shell rc path, got: ${path}`);
});

test('getConfigFile — continue returns ~/.continue/config.json', () => {
  assert.equal(getConfigFile('continue'), '~/.continue/config.json');
});

test('getConfigFile — aider returns undefined (env-only)', () => {
  assert.equal(getConfigFile('aider'), undefined);
});

test('getConfigFile — cursor returns undefined (GUI)', () => {
  assert.equal(getConfigFile('cursor'), undefined);
});

test('getConfigFile — cline returns undefined (GUI)', () => {
  assert.equal(getConfigFile('cline'), undefined);
});

test('getConfigFile — windsurf returns undefined (GUI)', () => {
  assert.equal(getConfigFile('windsurf'), undefined);
});

test('getConfigFile — antigravity returns undefined (GUI)', () => {
  assert.equal(getConfigFile('antigravity'), undefined);
});

test('getConfigFile — openclaw returns undefined (GUI)', () => {
  assert.equal(getConfigFile('openclaw'), undefined);
});

test('getConfigFile — opencode returns undefined (GUI)', () => {
  assert.equal(getConfigFile('opencode'), undefined);
});

test('getConfigFile — generic returns undefined (example code, not config)', () => {
  assert.equal(getConfigFile('generic'), undefined);
});

test('getConfigFile — unknown tool id returns undefined (defensive default)', () => {
  assert.equal(getConfigFile('not-a-real-tool'), undefined);
});
