import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { StatusDot } from './status-dot';
import { addToast } from './toast';
import { testConnection, deleteConnection, updateConnection } from '../api/client';
import { startOAuthFlow, getOAuthStatus, TOS_GATE_STATUS } from '../api/oauth';

// "Re-authenticate" → "Switch to API key" is taken when last_error
// signals provider rejection rather than a transient/refresh failure.
// Markers per ADR §R3 mitigation. Match case-insensitively because
// upstream error text varies (e.g. OAuth `invalid_client`, plain
// "blocked", Anthropic-specific phrasings).
const SWITCH_TO_API_KEY_MARKERS = ['invalid_client', 'invalid client', 'blocked', 'forbidden', 'unauthorized client'];

function shouldOfferSwitchToApiKey(lastError) {
  if (!lastError) return false;
  const lower = lastError.toLowerCase();
  return SWITCH_TO_API_KEY_MARKERS.some(m => lower.includes(m));
}

function formatExpiry(expiresAt, now) {
  if (!expiresAt) return null;
  const ms = new Date(expiresAt).getTime() - now;
  if (ms <= 0) return 'expired';
  const mins = Math.floor(ms / 60000);
  if (mins < 60) return `expires in ${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `expires in ${hours}h ${mins % 60}m`;
  const days = Math.floor(hours / 24);
  return `expires in ${days}d ${hours % 24}h`;
}

function stateToStatus(state) {
  if (state === 'idle' || state === 'active') return 'active';
  if (state === 'cooldown' || state === 'rate_limited' || state === 'refreshing') return 'cooldown';
  return 'error';
}

// Switch-to-API-key inline form. Shown when a subscription connection
// has been blocked and re-auth would just fail again — the user
// converts the row in place to an apikey connection.
function SwitchToApiKeyForm({ conn, onDone, onCancel }) {
  const key = signal('');
  const busy = signal(false);
  return (
    <div style={{
      marginTop: 8, padding: 'var(--space-md)',
      background: 'var(--bg-2)', border: '1px solid var(--border)',
      borderRadius: 'var(--radius-md)',
    }}>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 8 }}>
        Subscription appears to be blocked. Enter an API key to keep this
        connection working.
      </div>
      <input
        type="password"
        placeholder="sk-..."
        value={key.value}
        onInput={e => { key.value = e.target.value; }}
        style={{
          width: '100%', padding: '6px 10px', background: 'var(--bg-1)',
          border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
          color: 'var(--text-primary)', fontSize: 12, fontFamily: 'var(--font-mono)',
          marginBottom: 8,
        }}
      />
      <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
        <button
          onClick={onCancel}
          style={{
            padding: '4px 10px', fontSize: 11, color: 'var(--text-secondary)',
            background: 'var(--bg-1)', border: '1px solid var(--border)',
            borderRadius: 'var(--radius-sm)', cursor: 'pointer',
          }}
        >
          Cancel
        </button>
        <button
          disabled={busy.value || !key.value.trim()}
          onClick={() => {
            busy.value = true;
            updateConnection(conn.id, { auth_type: 'apikey', api_key: key.value }).then(() => {
              addToast('Switched to API key', 'success');
              onDone();
            }).catch(err => {
              busy.value = false;
              addToast('Switch failed: ' + err.message, 'error');
            });
          }}
          style={{
            padding: '4px 10px', fontSize: 11, color: 'var(--text-primary)',
            background: 'var(--accent)', border: 'none',
            borderRadius: 'var(--radius-sm)',
            cursor: busy.value || !key.value.trim() ? 'not-allowed' : 'pointer',
            opacity: busy.value || !key.value.trim() ? 0.5 : 1,
          }}
        >
          Use API key
        </button>
      </div>
    </div>
  );
}

// Re-authenticate a subscription connection: kick off a new PKCE flow
// for the same provider, then poll status. On success, replace the
// disabled connection's tokens; on this scope we leave the old row in
// place and the user can delete it after the new row appears (matches
// the existing "multiple subscription connections of same provider" model).
function reauthenticate(provider) {
  startOAuthFlow({ provider, name: '', priority: 0 }).then(res => {
    if (res.status === TOS_GATE_STATUS && res.data?.requires_tos) {
      addToast('Open Add Provider to accept the terms first', 'warning');
      return;
    }
    if (res.status === 503) {
      addToast(res.data?.hint || 'bridge port unavailable', 'error');
      return;
    }
    if (!res.ok) {
      addToast(res.data?.error || `start failed (${res.status})`, 'error');
      return;
    }
    const { authorize_url, state } = res.data;
    window.open(authorize_url, '_blank', 'noopener,noreferrer');
    addToast('Complete login in the new tab', 'info');
    // Fire-and-forget poll — UI will refresh when the user reloads /
    // navigates back. We avoid leaking timers across rows.
    const timer = setInterval(() => {
      getOAuthStatus(state).then(s => {
        if (!s.ok || !s.data) return;
        if (s.data.status === 'complete' || s.data.status === 'error') {
          clearInterval(timer);
          if (s.data.status === 'complete') {
            addToast('Re-authenticated — new connection created', 'success');
            window.dispatchEvent(new CustomEvent('sage:connection-added'));
          } else {
            addToast('Re-auth failed: ' + (s.data.error || 'unknown'), 'error');
          }
        }
      }).catch(() => {});
    }, 2000);
    // Belt-and-braces: stop polling after 15 minutes (matches bridge
    // pendingFlows TTL doubled for safety).
    setTimeout(() => clearInterval(timer), 15 * 60 * 1000);
  }).catch(err => { addToast('Re-auth failed: ' + err.message, 'error'); });
}

// ConnectionRow — renders one connection (a single account under a
// provider). For subscriptions, surfaces AuthType badge, account info,
// expiry countdown (refreshed every 30s), refresh_failures (when >0),
// and a Re-authenticate / Switch-to-API-key action when degraded.
export function ConnectionRow({ conn, onChanged }) {
  const tick = signal(Date.now());
  const showSwitch = signal(false);

  useEffect(() => {
    // 30s tick for the expiry countdown. Only schedule when there's
    // something to count down toward.
    if (!conn.expires_at) return;
    const timer = setInterval(() => { tick.value = Date.now(); }, 30000);
    return () => clearInterval(timer);
    // eslint-disable-next-line
  }, [conn.expires_at]);

  const authType = conn.auth_type || 'apikey';
  const isSubscription = authType === 'subscription' || authType === 'auto_detect';
  const status = stateToStatus(conn.state || 'idle');
  const expiryLabel = isSubscription ? formatExpiry(conn.expires_at, tick.value) : null;
  const accountInfo = conn.account_id || (isSubscription && conn.name !== conn.provider ? conn.name : '');
  const degraded = ['errored', 'disabled', 'auth_expired'].includes(conn.state);
  const offerSwitch = degraded && isSubscription && shouldOfferSwitchToApiKey(conn.last_error);

  const handleTest = () => {
    addToast(`Testing ${conn.name}…`, 'info');
    testConnection(conn.id).then(() => addToast('Connection OK', 'success'))
      .catch(err => addToast('Test failed: ' + err.message, 'error'));
  };

  const handleDelete = () => {
    if (!confirm(`Delete ${conn.name}?`)) return;
    deleteConnection(conn.id).then(() => {
      addToast('Deleted', 'success');
      onChanged();
    }).catch(err => addToast('Delete failed: ' + err.message, 'error'));
  };

  return (
    <div style={{
      padding: '10px var(--space-lg)',
      borderBottom: '1px solid var(--border)',
    }}>
      <div style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10,
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0, flex: 1 }}>
          <StatusDot status={status} pulse={status === 'cooldown'} />
          <span style={{ fontSize: 13, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
            {conn.name || conn.id}
          </span>
          {/* AuthType badge */}
          <span style={{
            fontSize: 10, fontFamily: 'var(--font-mono)',
            color: isSubscription ? 'var(--accent)' : 'var(--text-tertiary)',
            background: isSubscription ? 'var(--accent-muted)' : 'var(--bg-2)',
            padding: '2px 6px', borderRadius: 'var(--radius-sm)',
            border: '1px solid ' + (isSubscription ? 'var(--accent)' : 'var(--border)'),
            whiteSpace: 'nowrap',
          }}>
            {isSubscription ? 'Subscription' : 'API key'}
          </span>
          {accountInfo && isSubscription && (
            <span style={{ fontSize: 11, color: 'var(--text-tertiary)', whiteSpace: 'nowrap' }}>
              {accountInfo}
            </span>
          )}
          {expiryLabel && (
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)',
              color: expiryLabel === 'expired' ? 'var(--status-red)' : 'var(--text-tertiary)',
              whiteSpace: 'nowrap',
            }}>
              {expiryLabel}
            </span>
          )}
          {conn.refresh_failures > 0 && (
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--status-yellow)',
              whiteSpace: 'nowrap',
            }}
            title="Consecutive refresh failures since last success">
              {conn.refresh_failures}× refresh fail
            </span>
          )}
          {/* Cycle 20260516-connection-runtime-state: surface Selector runtime
              view so the dashboard can detect DB-vs-runtime drift + show
              per-model filter state that's otherwise invisible. All chips
              guard on field presence so old-server responses render as today. */}
          {conn.runtime_state && conn.runtime_state !== conn.state && (
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--status-yellow)',
              whiteSpace: 'nowrap',
            }}
            title={`Selector runtime state differs from DB. DB: ${conn.state} / Runtime: ${conn.runtime_state}. This usually means a transition (rate-limit, error, refresh) hasn't been persisted to the DB yet — common after crash/restart, or for in-flight requests. The Selector uses the runtime value for routing decisions.`}>
              Runtime: {conn.runtime_state}
            </span>
          )}
          {conn.model_denylist && Object.keys(conn.model_denylist).length > 0 && (
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--status-red)',
              whiteSpace: 'nowrap',
            }}
            title={`Models denylisted on this connection (1h TTL after a 401/403 model rejection):\n${
              Object.entries(conn.model_denylist)
                .map(([model, expiry]) => `${model} → expires ${new Date(expiry).toLocaleTimeString()}`)
                .join('\n')
            }\n\nRequests for these models will refuse this connection until the entry expires.`}>
              🚫 {Object.keys(conn.model_denylist).length} denylisted
            </span>
          )}
          {conn.model_locks && Object.keys(conn.model_locks).length > 0 && (
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--status-yellow)',
              whiteSpace: 'nowrap',
            }}
            title={`Models rate-limited on this connection (set after a 429 with backoff cooldown):\n${
              Object.entries(conn.model_locks)
                .map(([model, expiry]) => `${model} → unlocks ${new Date(expiry).toLocaleTimeString()}`)
                .join('\n')
            }\n\nRequests for these models will refuse this connection until the lock expires.`}>
              ⏳ {Object.keys(conn.model_locks).length} rate-limited
            </span>
          )}
        </div>
        <div style={{ display: 'flex', gap: 6 }}>
          {degraded && isSubscription && (
            offerSwitch ? (
              <button
                onClick={() => { showSwitch.value = true; }}
                style={{
                  fontSize: 11, color: 'var(--text-primary)',
                  padding: '3px 8px', background: 'var(--accent)',
                  border: 'none', borderRadius: 'var(--radius-sm)',
                  cursor: 'pointer', fontWeight: 500,
                }}
              >
                Switch to API key
              </button>
            ) : (
              <button
                onClick={() => reauthenticate(conn.provider)}
                style={{
                  fontSize: 11, color: 'var(--text-primary)',
                  padding: '3px 8px', background: 'var(--accent)',
                  border: 'none', borderRadius: 'var(--radius-sm)',
                  cursor: 'pointer', fontWeight: 500,
                }}
              >
                Re-authenticate
              </button>
            )
          )}
          <button
            onClick={handleTest}
            style={{
              fontSize: 11, color: 'var(--text-tertiary)',
              padding: '3px 8px', background: 'var(--bg-2)',
              border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
              cursor: 'pointer',
            }}
          >
            Test
          </button>
          <button
            onClick={handleDelete}
            style={{
              fontSize: 11, color: 'var(--text-tertiary)',
              padding: '3px 8px', background: 'var(--bg-2)',
              border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
              cursor: 'pointer',
            }}
          >
            Delete
          </button>
        </div>
      </div>
      {conn.last_error && degraded && !showSwitch.value && (
        <div style={{
          marginTop: 6, fontSize: 11, color: 'var(--status-red)',
          fontFamily: 'var(--font-mono)', wordBreak: 'break-word',
        }}>
          {conn.last_error}
        </div>
      )}
      {showSwitch.value && (
        <SwitchToApiKeyForm
          conn={conn}
          onDone={() => { showSwitch.value = false; onChanged(); }}
          onCancel={() => { showSwitch.value = false; }}
        />
      )}
    </div>
  );
}
