const BASE = '/api';

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function request(path, options = {}) {
  const { method = 'GET', body, headers: extra = {} } = options;
  const headers = { ...extra };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }

  const res = await fetch(`${BASE}${path}`, {
    method,
    headers,
    credentials: 'same-origin',
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (res.status === 401) {
    // Redirect to login or emit event
    window.dispatchEvent(new CustomEvent('sage:unauthorized'));
    throw new ApiError(401, 'Unauthorized');
  }

  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new ApiError(res.status, text);
  }

  if (res.status === 204) return null;

  return res.json();
}

// Auth
export function login(password) {
  return request('/auth/login', { method: 'POST', body: { password } });
}

export function authCheck() {
  return request('/auth/check');
}

export function logout() {
  return request('/auth/logout', { method: 'POST' });
}

// Connections (providers)
export function getConnections() {
  return request('/connections');
}

export function createConnection(data) {
  return request('/connections', { method: 'POST', body: data });
}

export function updateConnection(id, data) {
  return request(`/connections/${id}`, { method: 'PUT', body: data });
}

export function deleteConnection(id) {
  return request(`/connections/${id}`, { method: 'DELETE' });
}

export function testConnection(id) {
  return request(`/connections/${id}/test`, { method: 'POST' });
}

// Combos
export function getCombos() {
  return request('/combos');
}

export function createCombo(data) {
  return request('/combos', { method: 'POST', body: data });
}

export function updateCombo(id, data) {
  return request(`/combos/${id}`, { method: 'PUT', body: data });
}

export function deleteCombo(id) {
  return request(`/combos/${id}`, { method: 'DELETE' });
}

// Aliases
export function getAliases() {
  return request('/aliases');
}

export function setAlias(data) {
  return request('/aliases', { method: 'POST', body: data });
}

export function deleteAlias(name) {
  return request(`/aliases/${encodeURIComponent(name)}`, { method: 'DELETE' });
}

// API Keys
export function getKeys() {
  return request('/keys');
}

export function createKey(data) {
  return request('/keys', { method: 'POST', body: data });
}

export function updateKey(id, data) {
  return request(`/keys/${id}`, { method: 'PATCH', body: data });
}

export function deleteKey(id) {
  return request(`/keys/${id}`, { method: 'DELETE' });
}

// Settings
export function getSettings() {
  return request('/settings');
}

export function updateSettings(data) {
  return request('/settings', { method: 'PUT', body: data });
}

// Usage
export function getUsage(params = {}) {
  const qs = new URLSearchParams(params).toString();
  return request(`/usage${qs ? '?' + qs : ''}`);
}

export function getUsageSummary(params = {}) {
  const qs = new URLSearchParams(params).toString();
  return request(`/usage/summary${qs ? '?' + qs : ''}`);
}

// Token login (one-time setup token from terminal URL)
export function tokenLogin(token) {
  return request(`/auth/token-login?token=${encodeURIComponent(token)}`);
}

// First-run password setup
export function setupPassword(password) {
  return request('/auth/setup', { method: 'POST', body: { password } });
}

// Status
export function getStatus() {
  return request('/status');
}

// Claude credential detection
export function detectClaude() {
  return request('/detect/claude');
}

// Providers & Models (catalog)
export function getProviders() {
  return request('/providers');
}

export function getModels() {
  return request('/models');
}

// Models Discovery M2.10 — new /api/catalog/* endpoints.
//
// getCatalogModels returns the full catalog (every row, every source —
// distinct from getModels() which filters to active connections).
// Pass a provider string to filter; pass nothing for all.
export function getCatalogModels(provider) {
  return provider ? request(`/catalog/models/${encodeURIComponent(provider)}`) : request('/catalog/models');
}

// putCatalogPricing writes a user pricing override. PUT is strictly
// replace-not-patch — the body MUST include all five price fields
// (input_price, output_price, cache_read_price, cache_write_price,
// thinking_price). The handler rejects partial bodies with 400 + the
// missing field name. Caller is responsible for round-tripping the
// current state when editing one field.
export function putCatalogPricing(provider, modelId, pricing) {
  // model_id may contain a slash for OpenRouter (e.g. anthropic/claude-sonnet-4);
  // the route uses Go 1.22+ {model_id...} catch-all, so we must NOT
  // encode the slash. Encode each segment instead.
  const path = `/catalog/pricing/${encodeURIComponent(provider)}/${modelId.split('/').map(encodeURIComponent).join('/')}`;
  return request(path, { method: 'PUT', body: pricing });
}

// deleteCatalogPricing removes a user pricing override. Idempotent —
// 200 OK even when the row doesn't exist.
export function deleteCatalogPricing(provider, modelId) {
  const path = `/catalog/pricing/${encodeURIComponent(provider)}/${modelId.split('/').map(encodeURIComponent).join('/')}`;
  return request(path, { method: 'DELETE' });
}

// getCatalogProviders returns the catalog_provider_meta rows: per-
// provider discovery-loop state (enabled, last_discovered_at, error,
// backoff_step, next_discovery_after). Distinct from getProviders()
// which dumps the static KnownProviders map.
export function getCatalogProviders() {
  return request('/catalog/providers');
}

// Routing analytics
export function getRoutingSummary(params = {}) {
  const qs = new URLSearchParams(params).toString();
  return request(`/routing/summary${qs ? '?' + qs : ''}`);
}

export function getRoutingLog(params = {}) {
  const qs = new URLSearchParams(params).toString();
  return request(`/routing/log${qs ? '?' + qs : ''}`);
}
