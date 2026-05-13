import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { ConnectionAddModal } from '../components/connection-add-modal';
import { ConnectionRow } from '../components/connection-row';
import { addToast } from '../components/toast';
import { fmtTimestamp } from '../utils/format';
import { getConnections, getProviders } from '../api/client';

// providers.value holds the grouped list — { id, name, type, accounts: [...connections] }.
// Each "account" is the full Connection object from /api/connections so
// the row can render auth_type, expires_at, account_id, last_error, etc.
const providers = signal([]);
const showAddModal = signal(false);

// Models Discovery M2.12 (AC22) — providerMeta is a per-provider map
// of the discovery-loop state from /api/providers (which now merges
// catalog_provider_meta into config.KnownProviders). Read separately
// from connections so the page renders incrementally — connections
// load first (familiar), discovery state hydrates after.
const providerMeta = signal({});

function groupByProvider(connections) {
  const map = {};
  for (const c of connections) {
    if (!map[c.provider]) {
      map[c.provider] = { id: c.provider, name: c.provider, type: c.provider, accounts: [] };
    }
    map[c.provider].accounts.push(c);
  }
  return Object.values(map);
}

// loadAll fetches providerMeta and connections in parallel so the
// page renders both surfaces in one paint instead of letting the
// badges pop in ~100ms after the cards (carryover #28). Previously
// loadConnections and loadProviderMeta were called sequentially; the
// providerMeta fetch was the slower of the two, and ProviderCard
// rendered the cards before the discovery-state badges had data.
function loadAll() {
  Promise.all([
    getConnections().catch(err => {
      // Surface non-silent errors per loadProviderMeta's policy.
      // Silent (401) errors are handled by the sage:unauthorized
      // navigation; an extra toast flashes before the redirect.
      if (!err.silent) {
        addToast('Failed to load connections: ' + err.message, 'error');
      }
      return null;
    }),
    getProviders().catch(err => {
      if (!err.silent) {
        addToast('Failed to load provider meta: ' + err.message, 'error');
      }
      return null;
    }),
  ]).then(([connections, meta]) => {
    if (Array.isArray(connections)) {
      providers.value = groupByProvider(connections);
    }
    if (meta && typeof meta === 'object' && !Array.isArray(meta)) {
      providerMeta.value = meta;
    }
  });
}

// loadConnections refreshes connections only, after a per-card
// mutation (delete, re-auth, etc.). The providerMeta surface lags
// the connection set by at most one full page reload — it doesn't
// re-fetch here because nothing in ProviderCard's mutation surface
// changes the discovery state.
function loadConnections() {
  getConnections().then(data => {
    if (Array.isArray(data)) {
      providers.value = groupByProvider(data);
    }
  }).catch(err => {
    if (err.silent) return;
    addToast('Failed to load connections: ' + err.message, 'error');
  });
}

function ProviderCard({ provider, meta, onChanged }) {
  // Discovery state — M2.12 (AC22). meta is the merged providerListEntry
  // from /api/providers (may be undefined if /api/providers hasn't
  // loaded yet or the provider isn't in KnownProviders).
  const discoveryEnabled = meta?.discovery_enabled === true;
  const lastErrRaw = meta?.last_discovery_error || '';
  // Truncate the tooltip text to ~200 chars (M2.12 review MINOR-2).
  // last_discovery_error is plain text from the discovery runner; in
  // practice it's short ("empty model list", "HTTP 502"), but the
  // contract isn't enforced upstream — a future lister could capture
  // a multi-KB upstream error body and the tooltip would become
  // unreadable.
  const lastErr = lastErrRaw.length > 200 ? lastErrRaw.slice(0, 200) + '…' : lastErrRaw;
  const backoff = meta?.backoff_step || 0;
  const lastAt = meta?.last_discovered_at || '';

  return (
    <div style={{
      background: 'var(--bg-1)',
      border: '1px solid var(--border)',
      borderRadius: 'var(--radius-lg)',
      overflow: 'hidden',
    }}>
      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: 'var(--space-lg)',
        borderBottom: '1px solid var(--border)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
          <span style={{ fontWeight: 500, fontSize: 14 }}>{provider.name}</span>
          <span style={{
            fontSize: 10,
            fontFamily: 'var(--font-mono)',
            color: 'var(--text-tertiary)',
            background: 'var(--bg-2)',
            padding: '2px 6px',
            borderRadius: 'var(--radius-sm)',
          }}>
            {provider.type}
          </span>
          {/* Discovery state badge. Surfaces three operational signals:
              (a) discovery_enabled=false → "discovery off" (e.g. github-copilot
                  uses the static M3 allowlist exclusively);
              (b) last_discovery_error non-empty → red error badge with tooltip;
              (c) backoff_step > 0 → amber backoff indicator. */}
          {meta && !discoveryEnabled && (
            <span style={{
              fontSize: 10, fontFamily: 'var(--font-mono)',
              color: 'var(--text-tertiary)', background: 'var(--bg-2)',
              padding: '2px 6px', borderRadius: 'var(--radius-sm)',
            }} title="discovery_enabled=false — provider uses static allowlist">
              discovery off
            </span>
          )}
          {meta && discoveryEnabled && lastErr && (
            <span style={{
              fontSize: 10, fontFamily: 'var(--font-mono)',
              color: 'var(--status-red)', background: 'var(--bg-2)',
              padding: '2px 6px', borderRadius: 'var(--radius-sm)',
            }} title={lastErr}>
              discovery error{backoff > 0 ? ` (backoff ${backoff})` : ''}
            </span>
          )}
          {meta && discoveryEnabled && !lastErr && backoff > 0 && (
            <span style={{
              fontSize: 10, fontFamily: 'var(--font-mono)',
              color: 'var(--status-yellow)', background: 'var(--bg-2)',
              padding: '2px 6px', borderRadius: 'var(--radius-sm)',
            }} title={`backoff step ${backoff}`}>
              backoff {backoff}
            </span>
          )}
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          {meta && discoveryEnabled && lastAt && (
            <span
              style={{ fontSize: 11, color: 'var(--text-tertiary)' }}
              title={`last_discovered_at: ${lastAt}`}
            >
              discovered {fmtTimestamp(lastAt)}
            </span>
          )}
          <span style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>
            {provider.accounts.length} account{provider.accounts.length !== 1 ? 's' : ''}
          </span>
        </div>
      </div>

      {provider.accounts.map(conn => (
        <ConnectionRow key={conn.id} conn={conn} onChanged={onChanged} />
      ))}
    </div>
  );
}

export function ProvidersPage() {
  useEffect(() => {
    loadAll();
  }, []);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 960, width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-xl)' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600 }}>Providers</h1>
        <button
          onClick={() => { showAddModal.value = true; }}
          style={{
            padding: '6px 14px',
            fontSize: 13,
            color: 'var(--text-primary)',
            background: 'var(--accent)',
            borderRadius: 'var(--radius-md)',
            cursor: 'pointer',
            fontWeight: 500,
          }}
        >
          + Add Provider
        </button>
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-md)' }}>
        {providers.value.length === 0 ? (
          <div style={{ padding: 'var(--space-xl)', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13, background: 'var(--bg-1)', border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)' }}>
            No providers configured. Click "+ Add Provider" to get started.
          </div>
        ) : providers.value.map(p => (
          <ProviderCard key={p.id} provider={p} meta={providerMeta.value[p.id]} onChanged={loadConnections} />
        ))}
      </div>

      {showAddModal.value && (
        <ConnectionAddModal
          onClose={() => { showAddModal.value = false; }}
          onAdded={loadConnections}
        />
      )}
    </div>
  );
}
