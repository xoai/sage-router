// Subscription-auth API client. Wraps the /api/auth/oauth/*,
// /api/auth/import, /api/auth/tos endpoints introduced in M2.
//
// Error model: every request returns {ok, status, data} so callers can
// branch on status without try/catch around the happy path. Status 412
// (requires_tos) and 503 (port_busy) are surfaced as data, not thrown.

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
