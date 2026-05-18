// model-picker-helpers — pure functions for the allowed-models picker.
//
// Pure JS, NO Preact/DOM imports. Tested via `node --test
// model-picker-helpers.test.js`. Backend matcher contract at
// internal/server/routes_v1.go:1684-1703 (`matchModelPattern`) is the
// authority — these helpers MUST produce strings that round-trip cleanly
// through that matcher.

// normalizeModels — bridges /api/models response shape into the picker's
// internal {id, owned_by, ...} canonical.
//
// /api/models routes to handleListModelCatalog at routes_api.go:1129-1166
// which emits a BARE ARRAY of {id, provider, display_name, input_price,
// output_price} entries. When the catalog has no active-provider rows the
// handler returns `null` (NOT `[]`) because `var models []modelEntry` at
// routes_api.go:1152 is a nil-slice and Go's JSON encoder emits null for
// nil-slices.
//
// The picker's existing code (and helpers' projectChips) reads m.owned_by
// for provider derivation. We preserve that internal canonical and
// translate `provider` → `owned_by` at the data boundary so no internal
// code change cascades.
//
// Defensive against:
//   - null/undefined response (empty catalog or fetch failure-handled-elsewhere)
//   - bare-array (current /api/models shape)
//   - wrapped {data:[...]} (hypothetical /v1/models OpenAI-compat shape)
//   - entries missing BOTH provider AND owned_by (filtered out — junk data)
//   - entries with owned_by="sage-router" (combos/aliases mixed in — filtered)
//
// Test pins live in model-picker-helpers.test.js — DO NOT delete those
// when refactoring; they prevent recurrence of the JS↔Go shape-drift bug
// that necessitated this function.
export function normalizeModels(raw) {
  const arr = Array.isArray(raw) ? raw : (raw?.data || []);
  return arr
    .map(m => ({ ...m, owned_by: m.provider || m.owned_by }))
    .filter(m => m.owned_by && m.owned_by !== 'sage-router');
}

const PROVIDER_DISPLAY = {
  anthropic: 'Anthropic',
  openai: 'OpenAI',
  gemini: 'Gemini',
  openrouter: 'OpenRouter',
  'github-copilot': 'GitHub Copilot',
  ollama: 'Ollama',
};

export function displayName(provider) {
  return PROVIDER_DISPLAY[provider] || provider;
}

export function parseEntries(value) {
  if (value == null || value === '') return [];
  return value.split(',').map(s => s.trim()).filter(Boolean);
}

// commit(currentValue, nextEntries) → string
//
// Pure: Set-dedupe, drop empty, comma-join. Returns the new value string.
// Caller invokes onChange separately.
//
// Order preservation: cycle 20260516-routing-strategy-ux M4 dropped the
// previous `result.sort()` step. The allowed_models comma-string IS the
// order users see in the picker (drag-drop reorder) and the order the
// router tries candidates when `routing_strategy=user-order`. Insertion
// order is preserved through dedupe (Set iteration order = first-seen).
//
// Floor invariant (cycle 20260516-picker-floor): `*` is the picker's
// default-unrestricted floor. Two state-machine transitions enforced here
// at the chokepoint so ALL callers (addEntry, addEntries, removeEntry,
// clearAllAvailable, and the free-text fallback path) inherit them:
//   1. Empty selection auto-restores `*` (returns `'*'`).
//   2. Any non-`*` entry coexisting with `*` strips `*` (transition
//      unrestricted-all → restricted-specific; no redundant coexistence).
// setAllAvailable() at the bottom of this file returns `'*'` directly
// (exempt — its output IS the floor state).
export function commit(_currentValue, nextEntries) {
  const unique = [...new Set(nextEntries.map(e => String(e).trim()).filter(Boolean))];
  if (unique.length === 0) return '*';
  const nonStar = unique.filter(e => e !== '*');
  const result = nonStar.length > 0 ? nonStar : unique;
  return result.join(',');
}

export function addEntry(currentValue, entry) {
  return commit(currentValue, [...parseEntries(currentValue), entry]);
}

export function addEntries(currentValue, entries) {
  return commit(currentValue, [...parseEntries(currentValue), ...entries]);
}

export function removeEntry(currentValue, entry) {
  return commit(currentValue, parseEntries(currentValue).filter(e => e !== entry));
}

// reorderEntries(currentValue, fromIndex, toIndex) → string
//
// Move the entry at `fromIndex` to `toIndex` within the parsed entries
// array, then commit() (NO sort). Used by the picker's drag-drop and
// keyboard ↑/↓ reorder paths. Out-of-range indices are silent no-ops
// (returns currentValue unchanged). Floor invariant is preserved via
// commit() (empty result → `*`).
//
// Cycle 20260516-routing-strategy-ux M4.
export function reorderEntries(currentValue, fromIndex, toIndex) {
  const entries = parseEntries(currentValue);
  if (fromIndex < 0 || fromIndex >= entries.length) return currentValue;
  if (toIndex < 0 || toIndex >= entries.length) return currentValue;
  if (fromIndex === toIndex) return currentValue;
  const [moved] = entries.splice(fromIndex, 1);
  entries.splice(toIndex, 0, moved);
  return commit(currentValue, entries);
}

export function setAllAvailable() {
  return '*';
}

export function clearAllAvailable(currentValue) {
  return commit(currentValue, parseEntries(currentValue).filter(e => e !== '*'));
}

// projectChips(value, models, combos) → Array<{key, label, kind, dangling, members?}>
//
// Empty value / single `*` → one `all` chip. Otherwise project each
// comma-separated entry. Kind discrimination:
//   - '*'                       → kind: 'all'
//   - 'provider/*'              → kind: 'wildcard'
//   - matching combo by name    → kind: 'combo' (members included)
//   - 'provider/exact-model-id' → kind: 'model'
//   - else                      → kind: 'other'
//
// Mid-string asterisks (e.g., 'openai/gpt-4*') are NOT matcher-supported
// (only trailing /* per routes_v1.go:1693-1697). They fall through to the
// exact-match branch and project as dangling 'model' kind — existing keys
// with this shape have always silently never matched anything; the picker
// surfaces this as visible dangling rather than hiding the issue.
export function projectChips(value, models, combos) {
  const entries = parseEntries(value);
  if (entries.length === 0 || (entries.length === 1 && entries[0] === '*')) {
    return [{ key: '*', label: 'All available models', kind: 'all', dangling: false }];
  }
  const modelMap = new Map(models.map(m => [m.id, m]));
  const providerSet = new Set(models.map(m => m.owned_by));
  const comboMap = new Map(combos.map(c => [c.name, c]));

  return entries.map(entry => {
    if (entry === '*') {
      return { key: entry, label: 'All available models', kind: 'all', dangling: false };
    }
    if (entry.endsWith('/*')) {
      const provider = entry.slice(0, -2);
      return {
        key: entry,
        label: `${displayName(provider)} — all models`,
        kind: 'wildcard',
        dangling: !providerSet.has(provider),
      };
    }
    if (comboMap.has(entry)) {
      const combo = comboMap.get(entry);
      const members = combo.models || [];
      const anyMemberActive = members.some(m => modelMap.has(m));
      return {
        key: entry,
        label: `combo: ${entry}`,
        kind: 'combo',
        members,
        dangling: members.length > 0 && !anyMemberActive,
      };
    }
    if (entry.includes('/')) {
      return {
        key: entry,
        label: entry,
        kind: 'model',
        dangling: !modelMap.has(entry),
      };
    }
    return {
      key: entry,
      label: entry,
      kind: 'other',
      dangling: !modelMap.has(entry),
    };
  });
}
