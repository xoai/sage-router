// connect-helpers.js — pure helpers for the Connect page (cycle
// 20260518-connect-page-guide).
//
// Two responsibilities:
// 1. Filter the model + combo catalogs by the selected key's
//    allowed_models. Wildcard '*' = unrestricted floor.
// 2. Map tool ids → config-file paths for the "Where to paste:" line.
//    Only tools with concrete filesystem paths return a value;
//    GUI-only tools and example-code tools return undefined.

// filterAllowed projects a catalog (models or combos) against the
// allowed_models entries from a key. Mirrors backend matchModelPattern
// at internal/server/routes_v1.go:2249-2265 — the 3-shape contract from
// model-picker.jsx:22:
//   1. '*'           — global wildcard, returns all items
//   2. 'provider/*'  — provider wildcard, matches any item whose
//                      .provider field starts the entry. Combos lack
//                      .provider so this branch is a no-op for them.
//   3. 'provider/id' — exact match against item[idField]
// Backend also matches when the candidate model id equals the provider
// portion of the pattern (`model == prefix`, routes_v1.go:2257). The
// dashboard catalog always emits `provider/slug` ids so this branch is
// unreachable from the current catalog, but we mirror it defensively
// to preserve full parity with backend semantics.
// When no entries match, returns empty array; when `allowed` is empty,
// returns empty array (no permissions).
export function filterAllowed(items, allowed, idField) {
  if (!Array.isArray(items)) return [];
  if (!Array.isArray(allowed)) return [];
  if (allowed.includes('*')) return items.slice();
  const set = new Set(allowed);
  // Pre-extract bare prefixes from provider/* patterns. Used by the
  // backend-parity branch below; computed once outside the per-item loop.
  const barePrefixes = new Set(
    allowed
      .filter(a => typeof a === 'string' && a.endsWith('/*'))
      .map(a => a.slice(0, -2)),
  );
  return items.filter(item => {
    if (set.has(item[idField])) return true;
    if (item.provider && set.has(item.provider + '/*')) return true;
    // Backend parity (routes_v1.go:2257 `model == prefix`): a bare id
    // equal to a pattern's prefix matches its `/*` wildcard. The
    // dashboard catalog always emits `provider/slug` ids so this is
    // unreachable today, but we mirror the contract shape-for-shape
    // so future catalog drift doesn't silently desync the UI from
    // the router.
    if (barePrefixes.has(item[idField])) return true;
    return false;
  });
}

// pickInitialSelection decides what value to assign to a
// single-select signal when the underlying filtered list changes.
// Stability: keep the current value if still in the filtered list,
// else fall back to the first filtered item, else empty string.
// Used by the auto-reset useEffect on key switch.
export function pickInitialSelection(currentValue, filteredItems, idField) {
  if (!Array.isArray(filteredItems) || filteredItems.length === 0) return '';
  if (currentValue && filteredItems.some(item => item[idField] === currentValue)) {
    return currentValue;
  }
  return filteredItems[0][idField] || '';
}

// getConfigFile returns the file path where a tool's config snippet
// should be pasted, or undefined for GUI-settings tools whose
// description already names the location. Aider returns undefined
// because its config-file path varies across versions; env-only
// configuration via shell rc is universal.
//
// Keep in sync with the `tools` array in connect.jsx — every tool id
// listed there must be handled (concretely or via the default
// undefined branch).
export function getConfigFile(toolId) {
  switch (toolId) {
    case 'claude-code':
    case 'codex':
      return '~/.zshrc or ~/.bashrc';
    case 'continue':
      return '~/.continue/config.json';
    default:
      return undefined;
  }
}
