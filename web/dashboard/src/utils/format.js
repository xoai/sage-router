// Shared formatting utilities for the dashboard. Avoid duplicating
// these in individual page files — when a format convention changes,
// it should change in one place.

// fmtTimestamp — render an RFC3339 timestamp as a short relative
// string ("2h ago", "3d ago", "—" when absent). Falls back to the raw
// date (YYYY-MM-DD) for ages older than 30 days so the operator sees
// the actual date for cold rows.
//
// Used by:
//   - pages/models.jsx (catalog row "Updated" column, M2.11)
//   - pages/providers.jsx (per-provider last_discovered_at, M2.12)
export function fmtTimestamp(s) {
  if (!s) return '—';
  const t = new Date(s);
  if (Number.isNaN(t.getTime())) return '—';
  const diffMs = Date.now() - t.getTime();
  const sec = Math.floor(diffMs / 1000);
  if (sec < 60) return 'just now';
  const min = Math.floor(sec / 60);
  if (min < 60) return min + 'm ago';
  const hr = Math.floor(min / 60);
  if (hr < 24) return hr + 'h ago';
  const day = Math.floor(hr / 24);
  if (day < 30) return day + 'd ago';
  return t.toISOString().slice(0, 10);
}

// fmtPrice — render a $/M-token price. Distinguishes legitimate zero
// (free model — operator may have explicitly priced it $0) from
// missing/undefined (no pricing data).
//   - undefined/null/NaN → '-' (no data)
//   - 0                  → '$0' (explicit free)
//   - 0 < v < 0.1        → '$0.XXXX' (sub-cent, 4 decimals)
//   - v ≥ 0.1            → '$X.XX'   (cents-and-up, 2 decimals)
export function fmtPrice(v) {
  if (v == null || Number.isNaN(v)) return '-';
  if (v === 0) return '$0';
  return '$' + (v < 0.1 ? v.toFixed(4) : v.toFixed(2));
}
