// Subscription-auth API client. Wraps the /api/auth/oauth/*,
// /api/auth/import, /api/auth/tos endpoints introduced in M2.
//
// Error model: every request returns {ok, status, data} so callers can
// branch on status without try/catch around the happy path. Status 428
// (requires_tos) and 503 (port_busy) are surfaced as data, not thrown.

// TOS_GATE_STATUS is the HTTP status the backend uses to indicate that
// the subscription-auth TOS must be acknowledged before the requested
// action proceeds. RFC 6585 `PreconditionRequired` (428), not
// `PreconditionFailed` (412). Source of truth on the Go side:
// `internal/server/routes_auth_oauth.go:63` (OAuth start) and `:188`
// (import). Tests pin both at `routes_auth_oauth_test.go:88,200` and
// `subscription_e2e_test.go:251`.
//
// Centralized here so the gate's status code lives in one place across
// the JS surface — drift between this and the Go side is what caused
// the original bug (the frontend checked 412 against the backend's
// 428, so the gate path silently fell through and the response message
// got dumped into a toast).
export const TOS_GATE_STATUS = 428;

const BASE = '/api';

async function call(path, { method = 'GET', body } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers,
    credentials: 'same-origin',
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    window.dispatchEvent(new CustomEvent('sage:unauthorized'));
  }
  let data = null;
  const text = await res.text();
  if (text) {
    try { data = JSON.parse(text); } catch { data = { message: text }; }
  }
  return { ok: res.ok, status: res.status, data };
}

export function getOAuthHealth() {
  return call('/auth/oauth/health');
}

export function rebindOAuthPort(provider) {
  return call('/auth/oauth/health/rebind', { method: 'POST', body: { provider } });
}

export function startOAuthFlow({ provider, name, priority }) {
  return call('/auth/oauth/start', {
    method: 'POST',
    body: { provider, name: name || '', priority: priority || 0 },
  });
}

export function getOAuthStatus(state) {
  return call(`/auth/oauth/status?state=${encodeURIComponent(state)}`);
}

export function importSubscription({ provider, name, priority, path }) {
  const body = { provider, name: name || '', priority: priority || 0 };
  if (path) body.path = path;
  return call('/auth/import', { method: 'POST', body });
}

export function acceptTOS() {
  return call('/auth/tos/accept', { method: 'POST' });
}
