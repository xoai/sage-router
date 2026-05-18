// usage-helpers.js — pure helpers used by usage.jsx.
//
// Two responsibilities (cycle 20260517-usage-page-filters):
// 1. Date-input → ISO-Z transformation (T8). The HTML5 <input type="date">
//    returns "YYYY-MM-DD" strings interpreted in browser-local timezone.
//    We convert to UTC at the day boundary, format byte-for-byte against
//    the server's timeStr storage format (no fractional seconds).
//    Memory anchor [82fa8ebabd]: SQLite TEXT comparisons are
//    lexicographic — any format drift silently makes range queries miss.
// 2. Selection-aware gating helpers (T10). Three pure functions decide
//    whether the breakdown table / budget bar / single-key resolution
//    apply based on `selectedKeys` Set size and `allKeys` array.

// toUTCStartOfDay converts a "YYYY-MM-DD" string (browser local-day) to
// an ISO-Z string at the start of that local day, expressed in UTC.
// Empty input returns "" so callers can `if (s) buildQS({from: s})`.
//
// Format: "YYYY-MM-DDTHH:MM:SSZ" (no fractional seconds — matches the
// server's `timeStr` helper at internal/store/sqlite.go:1021).
export function toUTCStartOfDay(s) {
  if (!s) return '';
  // new Date("2026-05-17") parses as UTC midnight; we want LOCAL midnight.
  // Constructing with separate parts forces local interpretation.
  const [y, m, d] = s.split('-').map(Number);
  const local = new Date(y, m - 1, d, 0, 0, 0, 0);
  return formatZNoFrac(local);
}

// toUTCEndOfDay converts a "YYYY-MM-DD" string to an ISO-Z string at
// the END of that local day (23:59:59), expressed in UTC. Mirrors
// toUTCStartOfDay for the upper bound.
export function toUTCEndOfDay(s) {
  if (!s) return '';
  const [y, m, d] = s.split('-').map(Number);
  const local = new Date(y, m - 1, d, 23, 59, 59, 0);
  return formatZNoFrac(local);
}

// formatZNoFrac drops sub-second precision and emits a Z-suffixed ISO
// string. Date.prototype.toISOString() includes ".sssZ" which would
// silently mismatch the server's lexicographic TEXT comparison.
function formatZNoFrac(d) {
  const pad = n => String(n).padStart(2, '0');
  return (
    d.getUTCFullYear() + '-' +
    pad(d.getUTCMonth() + 1) + '-' +
    pad(d.getUTCDate()) + 'T' +
    pad(d.getUTCHours()) + ':' +
    pad(d.getUTCMinutes()) + ':' +
    pad(d.getUTCSeconds()) + 'Z'
  );
}

// presetDateStrings returns the {from, to} "YYYY-MM-DD" strings for a
// "last N days" preset, in browser-local timezone. Exported so the
// preset chips in usage.jsx can hydrate the date inputs, and so the
// off-by-one month / padStart arithmetic is testable in isolation.
export function presetDateStrings(days) {
  const now = new Date();
  const past = new Date(now.getTime() - days * 24 * 60 * 60 * 1000);
  const fmt = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  return { from: fmt(past), to: fmt(now) };
}

// shouldShowBreakdown returns true when the Per-Key Breakdown table
// should render. AC9 semantics:
//   0 keys selected → show (all-key breakdown)
//   1 key selected → hide (budget bar takes over)
//   2+ keys → show (filtered to selected)
// Plus the existing guard: only meaningful when there's more than one key.
export function shouldShowBreakdown(selectedKeys, allKeys) {
  if (!selectedKeys || !allKeys) return false;
  if (allKeys.length <= 1) return false;
  return selectedKeys.size !== 1;
}

// shouldShowBudgetBar returns true when the single-key budget block
// should render. AC9b: only when exactly one key is selected AND that
// key has a positive monthly budget.
export function shouldShowBudgetBar(selectedKeys, allKeys) {
  const k = singleSelectedKey(selectedKeys, allKeys);
  return !!(k && k.budget_monthly > 0);
}

// singleSelectedKey returns the key object when exactly one key is
// selected, else null. Handles the orphan case (selected id no longer
// in allKeys — e.g., key deleted between fetches).
export function singleSelectedKey(selectedKeys, allKeys) {
  if (!selectedKeys || selectedKeys.size !== 1 || !allKeys) return null;
  const id = selectedKeys.values().next().value;
  return allKeys.find(k => k.id === id) || null;
}
