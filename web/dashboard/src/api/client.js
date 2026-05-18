const BASE = '/api';

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
    // Carryover #25 — when silent is true, callers should suppress
    // user-facing toasts. Used for 401 responses where the
    // sage:unauthorized event already triggers navigation; an extra
    // "Unauthorized" toast flashes momentarily before the redirect
    // and reads as noise. Default false.
    this.silent = false;
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
    // Redirect to login or emit event. Mark the error as silent so
    // page-load fetches don't flash a redundant "Unauthorized" toast
    // before the unauthorized handler navigates away.
    window.dispatchEvent(new CustomEvent('sage:unauthorized'));
    const err = new ApiError(401, 'Unauthorized');
    err.silent = true;
    throw err;
  }

  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    // Carryover #31 — parse the {error: {message}} envelope produced
    // by writeError in routes_v1.go. The pinned envelope shape is
    // codified by TestWriteError_EnvelopeShape on the Go side. Falls
    // back to the raw body text for legacy 4xx/5xx responses that
    // don't follow the envelope (e.g., reverse-proxy plain-text 502s).
    let message = text;
    try {
      const parsed = JSON.parse(text);
      if (parsed && parsed.error && typeof parsed.error.message === 'string') {
        message = parsed.error.message;
      }
    } catch (_) {
      // Body wasn't JSON — keep raw text as the message.
    }
    throw new ApiError(res.status, message);
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
//
// Cycle 20260516-keys-management-redesign: getKeys now accepts a params
// object and serializes via URLSearchParams. Server responds with
// `{items, total, limit, offset}` envelope (not a bare array) per the
// new pagination contract. Existing callers that call `getKeys()` with
// no args still work — empty URLSearchParams yields empty query string.
export function getKeys(params) {
  const qs = params && Object.keys(params).length
    ? '?' + new URLSearchParams(params).toString()
    : '';
  return request('/keys' + qs);
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
//
// buildQS serializes a params object to a URL query string, honoring
// array values by emitting repeated params (`api_key_id=A&api_key_id=B`).
// The bare `new URLSearchParams(obj)` constructor stringifies arrays as
// `[object Object]`-style values and silently breaks multi-key filtering.
// Cycle 20260517-usage-page-filters T7.
//
// Skipped values: null, undefined, empty string, empty array.
export function buildQS(params) {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v == null || v === '') continue;
    if (Array.isArray(v)) {
      if (v.length === 0) continue;
      v.forEach(x => qs.append(k, String(x)));
    } else {
      qs.append(k, String(v));
    }
  }
  return qs.toString();
}

export function getUsage(params = {}) {
  const qs = buildQS(params);
  return request(`/usage${qs ? '?' + qs : ''}`);
}

export function getUsageSummary(params = {}) {
  const qs = buildQS(params);
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

// Codex CLI credential detection — OpenAI parallel of detectClaude.
// Returns {found, subscription_type, expired}. The dashboard's
// connection-add-modal uses this to render the "Codex CLI detected"
// auto-detect card when the user selects OpenAI in the provider
// dropdown. Initiative 20260513-openai-autodetect.
export function detectCodex() {
  return request('/detect/codex');
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
  const qs = buildQS(params);
  return request(`/routing/summary${qs ? '?' + qs : ''}`);
}

export function getRoutingLog(params = {}) {
  const qs = buildQS(params);
  return request(`/routing/log${qs ? '?' + qs : ''}`);
}
