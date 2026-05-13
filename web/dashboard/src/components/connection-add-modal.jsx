import { signal, computed } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import {
  getOAuthHealth, rebindOAuthPort, startOAuthFlow, getOAuthStatus,
  importSubscription,
} from '../api/oauth';
import { createConnection, detectClaude, getConnections } from '../api/client';
import { addToast } from './toast';
import { TosModal } from './tos-modal';

// PKCE providers — only these expose a "Login with subscription" button
// because they have a working interactive OAuth flow with a localhost
// redirect bridge. Gemini and Copilot are import-only.
const PKCE_PROVIDERS = new Set(['openai', 'anthropic']);

// Providers that have a CLI credential file we know how to import.
const IMPORT_PROVIDERS = new Set(['openai', 'anthropic', 'gemini', 'github-copilot']);

// All providers we render in the dropdown. Order matches the spec
// (subscription-capable first, then API-key-only).
const PROVIDER_OPTIONS = [
  { id: 'anthropic',      label: 'Anthropic' },
  { id: 'openai',         label: 'OpenAI' },
  { id: 'gemini',         label: 'Google (Gemini)' },
  { id: 'github-copilot', label: 'GitHub Copilot' },
  { id: 'openrouter',     label: 'OpenRouter' },
  { id: 'ollama',         label: 'Ollama' },
];

// Module-level signals — mirror the providers.jsx pattern.
const provider = signal('anthropic');
const label = signal('');
const apiKey = signal('');
const importPath = signal('');
const claudeDetect = signal(null);
const claudeDisclosure = signal(false);
const oauthHealth = signal({ openai: 'available', anthropic: 'available' });
const flowState = signal(null);   // { state, authorize_url, status, conn_id, error }
const tosPrompt = signal(null);   // { message, retry: fn() }
const pendingAction = signal(false);

// Bridge-health polling identifier — cleared on modal close.
let healthTimer = null;
let statusTimer = null;

function startHealthPoll() {
  stopHealthPoll();
  const tick = () => {
    getOAuthHealth().then(res => {
      if (res.ok && res.data) oauthHealth.value = res.data;
    }).catch(() => { /* keep last known */ });
  };
  tick();
  healthTimer = setInterval(tick, 5000);
}

function stopHealthPoll() {
  if (healthTimer) { clearInterval(healthTimer); healthTimer = null; }
}

function stopStatusPoll() {
  if (statusTimer) { clearInterval(statusTimer); statusTimer = null; }
}

function resetState() {
  provider.value = 'anthropic';
  label.value = '';
  apiKey.value = '';
  importPath.value = '';
  claudeDetect.value = null;
  claudeDisclosure.value = false;
  flowState.value = null;
  tosPrompt.value = null;
  pendingAction.value = false;
  stopStatusPoll();
}

// Handle a 412 requires_tos response: stash the message + a retry
// thunk, then the parent renders the TosModal which calls onAccept →
// runs retry.
function handleTosGate(res, retry) {
  if (res.status === 412 && res.data?.requires_tos) {
    tosPrompt.value = {
      message: res.data.message || 'Subscription terms require acceptance.',
      retry,
    };
    return true;
  }
  return false;
}

function beginOAuthFlow(canonicalProvider) {
  const doStart = () => {
    pendingAction.value = true;
    startOAuthFlow({
      provider: canonicalProvider,
      name: label.value.trim(),
      priority: 0,
    }).then(res => {
      pendingAction.value = false;
      if (handleTosGate(res, () => beginOAuthFlow(canonicalProvider))) return;
      if (res.status === 503) {
        const hint = res.data?.hint || 'bridge port unavailable';
        addToast(hint, 'error');
        oauthHealth.value = { ...oauthHealth.value, [canonicalProvider]: 'port_busy' };
        return;
      }
      if (!res.ok) {
        addToast(res.data?.error || res.data?.message || `start failed (${res.status})`, 'error');
        return;
      }
      const { authorize_url, state } = res.data;
      flowState.value = { state, authorize_url, status: 'pending' };
      window.open(authorize_url, '_blank', 'noopener,noreferrer');
      pollFlowStatus(state);
    }).catch(err => {
      pendingAction.value = false;
      addToast('Could not start login: ' + err.message, 'error');
    });
  };
  doStart();
}

function pollFlowStatus(state) {
  stopStatusPoll();
  const tick = () => {
    getOAuthStatus(state).then(res => {
      if (!res.ok || !res.data) return;
      const status = res.data.status;
      if (status === 'complete') {
        stopStatusPoll();
        flowState.value = { ...flowState.value, status: 'complete', conn_id: res.data.conn_id };
        addToast('Subscription connected', 'success');
        // Caller (parent modal close + reload) is responsible.
        window.dispatchEvent(new CustomEvent('sage:connection-added'));
      } else if (status === 'error') {
        stopStatusPoll();
        flowState.value = { ...flowState.value, status: 'error', error: res.data.error };
        addToast('Login failed: ' + (res.data.error || 'unknown error'), 'error');
      }
    }).catch(() => { /* keep polling */ });
  };
  tick();
  statusTimer = setInterval(tick, 2000);
}

function doImport(canonicalProvider) {
  const run = () => {
    pendingAction.value = true;
    importSubscription({
      provider: canonicalProvider,
      name: label.value.trim(),
      priority: 0,
      path: importPath.value.trim() || undefined,
    }).then(res => {
      pendingAction.value = false;
      if (handleTosGate(res, () => doImport(canonicalProvider))) return;
      if (res.status === 404) {
        const msg = res.data?.error || 'credential file not found';
        const hint = res.data?.hint ? '\n' + res.data.hint : '';
        addToast(msg + hint, 'error');
        return;
      }
      if (!res.ok) {
        addToast(res.data?.error || res.data?.message || `import failed (${res.status})`, 'error');
        return;
      }
      addToast('Subscription imported', 'success');
      window.dispatchEvent(new CustomEvent('sage:connection-added'));
    }).catch(err => {
      pendingAction.value = false;
      addToast('Could not import: ' + err.message, 'error');
    });
  };
  run();
}

function doApiKey(canonicalProvider) {
  if (!apiKey.value.trim()) {
    addToast('API key is required', 'warning');
    return;
  }
  pendingAction.value = true;
  createConnection({
    provider: canonicalProvider,
    name: label.value.trim() || canonicalProvider,
    auth_type: 'apikey',
    api_key: apiKey.value,
  }).then(() => {
    pendingAction.value = false;
    addToast('Provider added', 'success');
    window.dispatchEvent(new CustomEvent('sage:connection-added'));
  }).catch(err => {
    pendingAction.value = false;
    addToast('Failed: ' + err.message, 'error');
  });
}

function doClaudeAutoDetect() {
  pendingAction.value = true;
  createConnection({
    provider: 'anthropic',
    name: 'Claude Code',
    auth_type: 'auto_detect',
  }).then(() => {
    pendingAction.value = false;
    addToast('Claude Code connected', 'success');
    window.dispatchEvent(new CustomEvent('sage:connection-added'));
  }).catch(err => {
    pendingAction.value = false;
    addToast('Failed: ' + err.message, 'error');
  });
}

function doRebind(canonicalProvider) {
  rebindOAuthPort(canonicalProvider).then(res => {
    if (res.ok && res.data) {
      oauthHealth.value = { ...oauthHealth.value, [canonicalProvider]: res.data.health };
      if (res.data.health === 'available') {
        addToast('Port re-bound', 'success');
      } else {
        addToast('Port still busy', 'warning');
      }
    }
  }).catch(err => { addToast('Rebind failed: ' + err.message, 'error'); });
}

// SubscriptionLoginButton — PKCE-capable providers only. Disabled when
// bridge health for that provider != "available"; shows the AC44c hint
// and a "Retry bind" affordance per AC44d.
function SubscriptionLoginButton({ canonical }) {
  const health = oauthHealth.value[canonical] || 'available';
  const available = health === 'available';
  const busy = pendingAction.value;
  return (
    <div style={{ marginBottom: 'var(--space-md)' }}>
      <button
        disabled={!available || busy}
        onClick={() => beginOAuthFlow(canonical)}
        style={{
          width: '100%', padding: '8px 14px', fontSize: 13, fontWeight: 500,
          color: 'var(--text-primary)',
          background: available ? 'var(--accent)' : 'var(--bg-3)',
          border: '1px solid ' + (available ? 'var(--accent)' : 'var(--border)'),
          borderRadius: 'var(--radius-md)',
          cursor: available && !busy ? 'pointer' : 'not-allowed',
          opacity: available && !busy ? 1 : 0.6,
          textAlign: 'left',
        }}
      >
        Login with {canonical === 'openai' ? 'ChatGPT' : 'Claude'} subscription
        <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 8 }}>
          PKCE OAuth via localhost
        </span>
      </button>
      {!available && (
        <div style={{
          marginTop: 6, padding: '6px 8px', fontSize: 11,
          background: 'var(--bg-2)', borderRadius: 'var(--radius-sm)',
          color: 'var(--text-tertiary)', lineHeight: 1.4,
        }}>
          Port {canonical === 'openai' ? '1455' : '53692'} is in use by another
          tool (likely the {canonical === 'openai' ? 'Codex' : 'Claude'} CLI).
          Stop it, then{' '}
          <button
            onClick={() => doRebind(canonical)}
            style={{
              padding: '1px 6px', fontSize: 11, background: 'transparent',
              border: '1px solid var(--border)', borderRadius: 4, cursor: 'pointer',
              color: 'var(--accent)',
            }}
          >
            Retry bind
          </button>
          .
        </div>
      )}
    </div>
  );
}

function ImportFromCLIButton({ canonical }) {
  const busy = pendingAction.value;
  const expectedPath = {
    openai: '~/.codex/auth.json',
    anthropic: '~/.claude/.credentials.json',
    gemini: '~/.gemini/oauth_creds.json',
    'github-copilot': '~/.config/github-copilot/hosts.json',
  }[canonical] || 'CLI credential file';
  return (
    <div style={{ marginBottom: 'var(--space-md)' }}>
      <button
        disabled={busy}
        onClick={() => doImport(canonical)}
        style={{
          width: '100%', padding: '8px 14px', fontSize: 13, fontWeight: 500,
          color: 'var(--text-primary)', background: 'var(--bg-2)',
          border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
          cursor: busy ? 'not-allowed' : 'pointer',
          opacity: busy ? 0.6 : 1, textAlign: 'left',
        }}
      >
        Import from local CLI
        <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 8, fontFamily: 'var(--font-mono)' }}>
          {expectedPath}
        </span>
      </button>
    </div>
  );
}

// FlowPending — shown while the user completes the OAuth flow in their
// browser. The bridge handler creates the connection row + redirects
// back to /dashboard/oauth-complete; status polling picks it up.
function FlowPending({ state }) {
  return (
    <div style={{
      padding: 'var(--space-md)', marginBottom: 'var(--space-md)',
      background: 'var(--accent-muted)', border: '1px solid var(--accent)',
      borderRadius: 'var(--radius-md)',
    }}>
      <div style={{ fontSize: 13, fontWeight: 500, marginBottom: 6 }}>
        Complete the login in your browser
      </div>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 8 }}>
        A new tab should have opened. If not, use this link:
      </div>
      <a
        href={state.authorize_url}
        target="_blank"
        rel="noopener noreferrer"
        style={{ fontSize: 11, color: 'var(--accent)', wordBreak: 'break-all', fontFamily: 'var(--font-mono)' }}
      >
        {state.authorize_url}
      </a>
      <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 8 }}>
        Waiting for the callback…
      </div>
    </div>
  );
}

// ConnectionAddModal — full add-provider flow. Wraps three paths:
//  1. PKCE OAuth (subscription) for OpenAI / Anthropic
//  2. CLI import for the four subscription providers
//  3. API key (existing path)
// Triggers TosModal on first subscription action via the 412 gate.
export function ConnectionAddModal({ onClose, onAdded }) {
  useEffect(() => {
    startHealthPoll();
    // Pull Claude auto-detect status alongside connection list (used to
    // decide whether to show the auto-detect card).
    Promise.all([detectClaude(), getConnections()]).then(([detect, conns]) => {
      const alreadyConnected = Array.isArray(conns) && conns.some(
        c => c.provider === 'anthropic' && c.auth_type === 'auto_detect'
      );
      claudeDetect.value = { ...detect, _alreadyConnected: alreadyConnected };
    }).catch(() => { claudeDetect.value = { found: false }; });

    const onAdd = () => {
      if (onAdded) onAdded();
      onClose();
      resetState();
    };
    window.addEventListener('sage:connection-added', onAdd);
    return () => {
      stopHealthPoll();
      stopStatusPoll();
      window.removeEventListener('sage:connection-added', onAdd);
    };
  }, []);

  const canonical = provider.value;
  const showPkce = PKCE_PROVIDERS.has(canonical);
  const showImport = IMPORT_PROVIDERS.has(canonical);
  const showApiKey = canonical !== 'github-copilot'; // Copilot has no API-key path
  const showAutoDetect = canonical === 'anthropic'
    && claudeDetect.value?.found
    && !claudeDetect.value._alreadyConnected;

  return (
    <>
      <div
        onClick={() => { onClose(); resetState(); }}
        style={{
          position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
          backdropFilter: 'blur(4px)', display: 'flex', alignItems: 'center',
          justifyContent: 'center', zIndex: 9000,
        }}
      >
        <div
          onClick={e => e.stopPropagation()}
          style={{
            background: 'var(--bg-1)', border: '1px solid var(--border-hover)',
            borderRadius: 'var(--radius-xl)', padding: 'var(--space-xl)',
            width: 460, maxWidth: '92vw', maxHeight: '90vh', overflow: 'auto',
          }}
        >
          <h2 style={{ fontSize: 16, fontWeight: 600, marginBottom: 'var(--space-lg)' }}>
            Add Provider
          </h2>

          <div style={{ marginBottom: 'var(--space-md)' }}>
            <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>
              Provider
            </label>
            <select
              value={canonical}
              onChange={e => { provider.value = e.target.value; }}
              style={{
                width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                color: 'var(--text-primary)', fontSize: 13,
              }}
            >
              {PROVIDER_OPTIONS.map(p => (
                <option key={p.id} value={p.id}>{p.label}</option>
              ))}
            </select>
          </div>

          <div style={{ marginBottom: 'var(--space-md)' }}>
            <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>
              Label <span style={{ color: 'var(--text-tertiary)' }}>(optional)</span>
            </label>
            <input
              type="text"
              placeholder="e.g. Primary"
              value={label.value}
              onInput={e => { label.value = e.target.value; }}
              style={{
                width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                color: 'var(--text-primary)', fontSize: 13,
              }}
            />
          </div>

          {/* Active OAuth flow */}
          {flowState.value?.status === 'pending' && (
            <FlowPending state={flowState.value} />
          )}

          {/* Subscription section */}
          {(showPkce || showImport) && !flowState.value && (
            <>
              <div style={{
                fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)',
                textTransform: 'uppercase', letterSpacing: '0.06em',
                marginBottom: 8, marginTop: 'var(--space-sm)',
              }}>
                Subscription
              </div>
              {showPkce && <SubscriptionLoginButton canonical={canonical} />}
              {showImport && <ImportFromCLIButton canonical={canonical} />}
            </>
          )}

          {/* Claude auto-detect (legacy convenience) */}
          {showAutoDetect && (
            <div style={{
              marginBottom: 'var(--space-md)', padding: 'var(--space-md)',
              background: 'var(--bg-2)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)',
            }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
                <span style={{ color: 'var(--status-green)', fontSize: 14 }}>&#10003;</span>
                <span style={{ fontSize: 13, fontWeight: 500 }}>Claude Code detected</span>
                {claudeDetect.value.subscription_type && (
                  <span style={{
                    fontSize: 10, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)',
                    background: 'var(--bg-3)', padding: '2px 6px', borderRadius: 'var(--radius-sm)',
                  }}>
                    {claudeDetect.value.subscription_type}
                  </span>
                )}
              </div>
              <label style={{
                display: 'flex', alignItems: 'flex-start', gap: 8, fontSize: 11,
                color: 'var(--text-secondary)', cursor: 'pointer', lineHeight: 1.4,
              }}>
                <input
                  type="checkbox"
                  checked={claudeDisclosure.value}
                  onChange={e => { claudeDisclosure.value = e.target.checked; }}
                  style={{ marginTop: 2 }}
                />
                <span>
                  I understand this uses Claude Code credentials. Anthropic's TOS restricts OAuth
                  tokens to Claude Code and Claude.ai. Anthropic does not officially support this.
                </span>
              </label>
              <button
                disabled={!claudeDisclosure.value || pendingAction.value}
                onClick={doClaudeAutoDetect}
                style={{
                  width: '100%', marginTop: 'var(--space-md)', padding: '6px 14px',
                  fontSize: 12, fontWeight: 500, color: 'var(--text-primary)',
                  background: claudeDisclosure.value ? 'var(--accent)' : 'var(--bg-3)',
                  borderRadius: 'var(--radius-md)',
                  cursor: claudeDisclosure.value ? 'pointer' : 'not-allowed',
                  opacity: claudeDisclosure.value ? 1 : 0.5,
                }}
              >
                Connect with Claude Code (auto-detect)
              </button>
            </div>
          )}

          {/* API key fallback */}
          {showApiKey && !flowState.value && (
            <>
              {(showPkce || showImport || showAutoDetect) && (
                <div style={{
                  fontSize: 11, color: 'var(--text-tertiary)', textAlign: 'center',
                  margin: 'var(--space-md) 0',
                }}>
                  — or use an API key —
                </div>
              )}
              <div style={{ marginBottom: 'var(--space-md)' }}>
                <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>
                  API Key
                </label>
                <input
                  type="password"
                  placeholder="sk-..."
                  value={apiKey.value}
                  onInput={e => { apiKey.value = e.target.value; }}
                  style={{
                    width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                    border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                    color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
                  }}
                />
              </div>
              <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
                <button
                  onClick={() => { onClose(); resetState(); }}
                  style={{
                    padding: '6px 14px', fontSize: 13, color: 'var(--text-secondary)',
                    background: 'var(--bg-2)', border: '1px solid var(--border)',
                    borderRadius: 'var(--radius-md)', cursor: 'pointer',
                  }}
                >
                  Cancel
                </button>
                <button
                  onClick={() => doApiKey(canonical)}
                  disabled={pendingAction.value}
                  style={{
                    padding: '6px 14px', fontSize: 13, color: 'var(--text-primary)',
                    background: 'var(--accent)', borderRadius: 'var(--radius-md)',
                    cursor: pendingAction.value ? 'not-allowed' : 'pointer', fontWeight: 500,
                    opacity: pendingAction.value ? 0.6 : 1,
                  }}
                >
                  Add API Key
                </button>
              </div>
            </>
          )}

          {/* Cancel-only footer for active flow / copilot-no-apikey case */}
          {(flowState.value || !showApiKey) && (
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 'var(--space-md)' }}>
              <button
                onClick={() => { onClose(); resetState(); }}
                style={{
                  padding: '6px 14px', fontSize: 13, color: 'var(--text-secondary)',
                  background: 'var(--bg-2)', border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)', cursor: 'pointer',
                }}
              >
                Close
              </button>
            </div>
          )}
        </div>
      </div>

      {tosPrompt.value && (
        <TosModal
          message={tosPrompt.value.message}
          onAccept={() => {
            const retry = tosPrompt.value.retry;
            tosPrompt.value = null;
            if (retry) retry();
          }}
          onCancel={() => { tosPrompt.value = null; }}
        />
      )}
    </>
  );
}
