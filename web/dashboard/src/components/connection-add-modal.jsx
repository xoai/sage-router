import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import {
  getOAuthHealth, rebindOAuthPort, startOAuthFlow, getOAuthStatus,
  importSubscription,
  TOS_GATE_STATUS,
} from '../api/oauth';
import { createConnection, detectClaude, detectCodex, getConnections } from '../api/client';
import { addToast } from './toast';
import { TosModal } from './tos-modal';

// PKCE providers — only these expose a "Login with subscription" button
// because they have a working interactive OAuth flow with a localhost
// redirect bridge. Gemini and Copilot are import-only.
//
// In-cycle scope cut (20260517-provider-auth-variants): anthropic
// subscription UI is hidden pending M3 (ClaudeMaxExecutor) ship. The
// PKCE flow is technically fixed by M0.8 (form→JSON token exchange,
// commit pending) but the executor still routes anthropic+subscription
// through the apikey-shape ClaudeExecutor which doesn't work for
// claude.ai consumer tokens. Re-add 'anthropic' to this Set once
// M3 ships the variant Executor. See .sage/work/20260517-provider-auth-variants/.
const PKCE_PROVIDERS = new Set(['openai']);

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

// Per-provider section-header text for the "import / subscription"
// region. Anthropic and OpenAI have actual subscription tiers; Gemini
// and Copilot import from a local CLI's OAuth credentials but have no
// subscription tier — naming reflects that (spec B1/B4).
const SUBSCRIPTION_HEADER = {
  'anthropic':      'Use your subscription',
  'openai':         'Use your subscription',
  'gemini':         'Use your CLI credentials',
  'github-copilot': 'Use your CLI credentials',
};

// Per-provider one-click button for non-PKCE providers (gemini, copilot).
// PKCE providers (openai, anthropic) use auto-detect cards with disclosure
// instead — they live under the same "use your subscription" section but
// render via Claude/CodexAutoDetectCard, not OneClickImportButton.
const CLI_IMPORT_BUTTON_LABEL = {
  'gemini':           'Use Gemini CLI credentials',
  'github-copilot':   'Import Copilot credentials',
};

// Default credential paths. Used as informational hints on cards/buttons
// (AC-2 sub-letter b). Detect API returns only {found, subscription_type}
// today — backend doesn't echo the matched path — so this is a typical
// default, not the actually-resolved path. WSL+Windows users may have a
// different layout, but the detect succeeds either way.
const CLI_CREDENTIAL_PATH = {
  'anthropic':        '~/.claude/.credentials.json',
  'openai':           '~/.codex/auth.json',
  'gemini':           '~/.gemini/oauth_creds.json',
  'github-copilot':   '~/.copilot/settings.json',
};

// Module-level signals — mirror the providers.jsx pattern.
const provider = signal('anthropic');
const label = signal('');
const apiKey = signal('');
const claudeDetect = signal(null);
const claudeDisclosure = signal(false);
// Codex CLI auto-detect parallels Claude's. The duplicated card needs
// its OWN checkbox-state signal so the two cards' disclosure states
// don't bleed into each other (a user who acknowledges Claude's
// disclosure shouldn't have OpenAI's checkbox pre-checked, and vice
// versa). Initiative 20260513-openai-autodetect.
const codexDetect = signal(null);
const codexDisclosure = signal(false);
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
  claudeDetect.value = null;
  claudeDisclosure.value = false;
  codexDetect.value = null;
  codexDisclosure.value = false;
  flowState.value = null;
  tosPrompt.value = null;
  pendingAction.value = false;
  stopStatusPoll();
}

// Handle a TOS_GATE_STATUS (428) requires_tos response: stash the
// message + a retry thunk, then the parent renders the TosModal which
// calls onAccept → runs retry.
function handleTosGate(res, retry) {
  if (res.status === TOS_GATE_STATUS && res.data?.requires_tos) {
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

// doImport — one-click default-path import for gemini and copilot.
// The custom-path UI was removed in 20260514-add-provider-ux (out of
// scope; CLI `auth import --provider X --path ...` covers power users).
// Backend uses the provider's default credential location when `path`
// is omitted from the request body (api/oauth.js:64-67).
function doImport(canonicalProvider) {
  pendingAction.value = true;
  importSubscription({
    provider: canonicalProvider,
    name: label.value.trim(),
    priority: 0,
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

// doCodexAutoDetect — OpenAI parallel of doClaudeAutoDetect. Initiative
// 20260513-openai-autodetect. Backend at routes_api.go:252-272 reads
// `~/.codex/auth.json`, populates tokens, and converts auth_type to
// subscription so this row behaves like an imported subscription
// from then on (refresh-loop handles token rotation).
function doCodexAutoDetect() {
  pendingAction.value = true;
  createConnection({
    provider: 'openai',
    name: 'Codex CLI',  // HARDCODED per spec; Label input is ignored, matches Claude's 'Claude Code'.
    auth_type: 'auto_detect',
  }).then(() => {
    pendingAction.value = false;
    addToast('Codex CLI connected', 'success');
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

// ClaudeAutoDetectCard — anthropic auto-detect with security disclosure.
// The trust checkbox is the AC-2 exception: auto-detect reads another
// tool's credential file, so the disclosure is load-bearing security UX,
// not modal-design noise.
function ClaudeAutoDetectCard() {
  const detected = claudeDetect.value;
  return (
    <div style={{
      marginBottom: 'var(--space-md)', padding: 'var(--space-md)',
      background: 'var(--bg-2)', border: '1px solid var(--border)',
      borderRadius: 'var(--radius-md)',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
        <span style={{ color: 'var(--status-green)', fontSize: 14 }}>&#10003;</span>
        <span style={{ fontSize: 13, fontWeight: 500 }}>Claude Code detected</span>
        {detected.subscription_type && (
          <span style={{
            fontSize: 10, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)',
            background: 'var(--bg-3)', padding: '2px 6px', borderRadius: 'var(--radius-sm)',
          }}>
            {detected.subscription_type}
          </span>
        )}
      </div>
      <div style={{
        fontSize: 11, color: 'var(--text-tertiary)',
        fontFamily: 'var(--font-mono)', marginBottom: 8,
      }}>
        {CLI_CREDENTIAL_PATH.anthropic}
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
        Connect with Claude Code
      </button>
    </div>
  );
}

// CodexAutoDetectCard — openai parallel of ClaudeAutoDetectCard.
// Initiative 20260513-openai-autodetect. Codex's auth.json has no tier
// field today so the badge usually doesn't render.
function CodexAutoDetectCard() {
  const detected = codexDetect.value;
  return (
    <div style={{
      marginBottom: 'var(--space-md)', padding: 'var(--space-md)',
      background: 'var(--bg-2)', border: '1px solid var(--border)',
      borderRadius: 'var(--radius-md)',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
        <span style={{ color: 'var(--status-green)', fontSize: 14 }}>&#10003;</span>
        <span style={{ fontSize: 13, fontWeight: 500 }}>Codex CLI detected</span>
        {detected.subscription_type && (
          <span style={{
            fontSize: 10, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)',
            background: 'var(--bg-3)', padding: '2px 6px', borderRadius: 'var(--radius-sm)',
          }}>
            {detected.subscription_type}
          </span>
        )}
      </div>
      <div style={{
        fontSize: 11, color: 'var(--text-tertiary)',
        fontFamily: 'var(--font-mono)', marginBottom: 8,
      }}>
        {CLI_CREDENTIAL_PATH.openai}
      </div>
      <label style={{
        display: 'flex', alignItems: 'flex-start', gap: 8, fontSize: 11,
        color: 'var(--text-secondary)', cursor: 'pointer', lineHeight: 1.4,
      }}>
        <input
          type="checkbox"
          checked={codexDisclosure.value}
          onChange={e => { codexDisclosure.value = e.target.checked; }}
          style={{ marginTop: 2 }}
        />
        <span>
          I understand this uses Codex CLI credentials. OpenAI's TOS restricts OAuth
          tokens to Codex CLI and ChatGPT. OpenAI does not officially support this.
        </span>
      </label>
      <button
        disabled={!codexDisclosure.value || pendingAction.value}
        onClick={doCodexAutoDetect}
        style={{
          width: '100%', marginTop: 'var(--space-md)', padding: '6px 14px',
          fontSize: 12, fontWeight: 500, color: 'var(--text-primary)',
          background: codexDisclosure.value ? 'var(--accent)' : 'var(--bg-3)',
          borderRadius: 'var(--radius-md)',
          cursor: codexDisclosure.value ? 'pointer' : 'not-allowed',
          opacity: codexDisclosure.value ? 1 : 0.5,
        }}
      >
        Connect with Codex CLI
      </button>
    </div>
  );
}

// OneClickImportButton — non-PKCE providers (gemini, copilot). One
// click imports OAuth credentials from the CLI's default path. No
// disclosure checkbox: these are the user's own CLI credentials,
// trust model = same as the API key they'd paste.
function OneClickImportButton({ canonical }) {
  const busy = pendingAction.value;
  const text = CLI_IMPORT_BUTTON_LABEL[canonical] || 'Use CLI credentials';
  const hint = CLI_CREDENTIAL_PATH[canonical] || '';
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
        {text}
        {hint && (
          <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 8, fontFamily: 'var(--font-mono)' }}>
            {hint}
          </span>
        )}
      </button>
    </div>
  );
}

// ApiKeyInput — paste-key form with IME-safe Enter-to-submit (AC-11).
// East Asian IMEs commit composition candidates on Enter; we must NOT
// fire submit during composition (keyCode === 229 covers older browsers,
// isComposing covers modern). Plan-review M3.
function ApiKeyInput({ canSubmit, onSubmit }) {
  return (
    <div style={{ marginBottom: 'var(--space-md)' }}>
      <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>
        API Key
      </label>
      <input
        type="password"
        placeholder="sk-..."
        value={apiKey.value}
        onInput={e => { apiKey.value = e.target.value; }}
        onKeyDown={e => {
          if (e.isComposing || e.keyCode === 229) return;
          if (e.key === 'Enter' && canSubmit) {
            e.preventDefault();
            onSubmit();
          }
        }}
        style={{
          width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
          border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
          color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
        }}
      />
    </div>
  );
}

// Footer — Cancel always; primary "Add provider" only on api-key path.
// AC-3 (primary submits api-key form), AC-4 (disabled on empty field).
function Footer({ showPrimary, primaryDisabled, onPrimary, onCancel }) {
  return (
    <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 'var(--space-md)' }}>
      <button
        onClick={onCancel}
        style={{
          padding: '6px 14px', fontSize: 13, color: 'var(--text-secondary)',
          background: 'var(--bg-2)', border: '1px solid var(--border)',
          borderRadius: 'var(--radius-md)', cursor: 'pointer',
        }}
      >
        Cancel
      </button>
      {showPrimary && (
        <button
          onClick={onPrimary}
          disabled={primaryDisabled}
          style={{
            padding: '6px 14px', fontSize: 13, color: 'var(--text-primary)',
            background: 'var(--accent)', borderRadius: 'var(--radius-md)',
            border: 'none',
            cursor: primaryDisabled ? 'not-allowed' : 'pointer', fontWeight: 500,
            opacity: primaryDisabled ? 0.6 : 1,
          }}
        >
          Add provider
        </button>
      )}
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

// ConnectionAddModal — full add-provider flow. Three entry paths:
//  1. PKCE OAuth (subscription) for OpenAI / Anthropic — imperative button
//  2. CLI auto-detect / one-click import for the four subscription
//     providers — imperative button (with disclosure for PKCE pair)
//  3. API key (declarative form, primary CTA at footer)
// 20260514-add-provider-ux redesigned the render structure: the primary
// CTA at the footer applies ONLY to the API-key path. Subscription paths
// fire on their own buttons. Triggers TosModal via the 428 gate.
export function ConnectionAddModal({ onClose, onAdded }) {
  useEffect(() => {
    startHealthPoll();
    // Pull auto-detect status for both Claude and Codex CLI alongside
    // the connection list. Detection fires ONCE at modal mount via
    // this Promise.all — visibility per provider is then a pure
    // dropdown-gate (no re-fetch on dropdown change).
    //
    // _alreadyConnected guards against showing the auto-detect card
    // when an existing auto_detect-typed connection already exists for
    // the same provider. Currently vacuous: the backend converts
    // auth_type=auto_detect to auth_type=subscription at create time
    // (routes_api.go:251,272), so no row ever persists as auto_detect.
    // Suppression is keyed on auth_type=auto_detect specifically —
    // forward-compat if the backend ever stops the create-time conversion.
    Promise.all([detectClaude(), detectCodex(), getConnections()]).then(([detectClaude_, detectCodex_, conns]) => {
      const claudeAlready = Array.isArray(conns) && conns.some(
        c => c.provider === 'anthropic' && c.auth_type === 'auto_detect'
      );
      const codexAlready = Array.isArray(conns) && conns.some(
        c => c.provider === 'openai' && c.auth_type === 'auto_detect'
      );
      claudeDetect.value = { ...detectClaude_, _alreadyConnected: claudeAlready };
      codexDetect.value = { ...detectCodex_, _alreadyConnected: codexAlready };
    }).catch(() => {
      claudeDetect.value = { found: false };
      codexDetect.value = { found: false };
    });

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
  const showApiKey = canonical !== 'github-copilot'; // Copilot has no API-key path
  // In-cycle scope cut (20260517-provider-auth-variants): hide the
  // anthropic Claude-Code auto-detect card until M3 ships the
  // ClaudeMaxExecutor variant. Flip back to:
  //   canonical === 'anthropic' && claudeDetect.value?.found
  //     && !claudeDetect.value._alreadyConnected
  // when M3 ships.
  const showAutoDetect = false;
  const showCodexAutoDetect = canonical === 'openai'
    && codexDetect.value?.found
    && !codexDetect.value._alreadyConnected;
  // One-click default-path import for non-PKCE providers. PKCE providers
  // use auto-detect with disclosure (AC-2 exception).
  const showOneClickImport = canonical === 'gemini' || canonical === 'github-copilot';
  // Subscription/credentials section appears when ANY one-click
  // alternative to API key is available for this provider.
  const showSubscriptionSection = showPkce || showAutoDetect || showCodexAutoDetect || showOneClickImport;
  // API-key submit gate: AC-4 (disabled when empty) AND AC-1/AC-2/AC-3
  // not in progress.
  const canSubmitApiKey = apiKey.value.trim().length > 0 && !pendingAction.value;

  const pending = flowState.value?.status === 'pending';
  const handleClose = () => { onClose(); resetState(); };
  const sectionHeader = text => (
    <div style={{
      fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)',
      textTransform: 'uppercase', letterSpacing: '0.06em',
      marginBottom: 8, marginTop: 'var(--space-sm)',
    }}>
      {text}
    </div>
  );

  return (
    <>
      <div
        onClick={handleClose}
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
              onChange={e => {
                provider.value = e.target.value;
                // Clear in-progress form state on provider switch (AC-10a:
                // "resets form state appropriately"). The trust-disclosure
                // checkboxes (AC-2 / AC-9) are load-bearing security UX —
                // a fresh consent is required after switching providers,
                // not a sticky check from before. Also clear the API-key
                // field so a key pasted for one provider doesn't get
                // submitted against another. Label persists (AC-10b).
                apiKey.value = '';
                claudeDisclosure.value = false;
                codexDisclosure.value = false;
              }}
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

          {/* Active OAuth flow replaces everything else (AC-7, AC-10c) */}
          {pending && <FlowPending state={flowState.value} />}

          {/* Subscription / CLI credentials section (AC-1, AC-2, AC-6) */}
          {!pending && showSubscriptionSection && (
            <>
              {sectionHeader(SUBSCRIPTION_HEADER[canonical])}
              {showPkce && <SubscriptionLoginButton canonical={canonical} />}
              {showAutoDetect && <ClaudeAutoDetectCard />}
              {showCodexAutoDetect && <CodexAutoDetectCard />}
              {showOneClickImport && <OneClickImportButton canonical={canonical} />}
            </>
          )}

          {/* API-key form (AC-3, AC-5, AC-6) */}
          {!pending && showApiKey && (
            <>
              {showSubscriptionSection && sectionHeader('Or use an API key')}
              <ApiKeyInput
                canSubmit={canSubmitApiKey}
                onSubmit={() => doApiKey(canonical)}
              />
            </>
          )}

          {/* Footer: primary CTA only on api-key path; Cancel always */}
          <Footer
            showPrimary={!pending && showApiKey}
            primaryDisabled={!canSubmitApiKey}
            onPrimary={() => doApiKey(canonical)}
            onCancel={handleClose}
          />
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
