import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { getRoutingSummary, getRoutingLog } from '../api/client';

const summary = signal(null);
const logEntries = signal([]);
const loading = signal(true);

function StatCard({ label, value, sub, color }) {
  return (
    <div style={{ flex: 1, minWidth: 140 }}>
      <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>{label}</div>
      <div style={{ fontSize: 24, fontFamily: 'var(--font-mono)', fontWeight: 600, color: color || 'var(--text-primary)', marginTop: 4 }}>{value}</div>
      {sub && <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 2 }}>{sub}</div>}
    </div>
  );
}

function StrategyBar({ data }) {
  if (!data || Object.keys(data).length === 0) {
    return <div style={{ color: 'var(--text-tertiary)', fontSize: 13 }}>No routing decisions yet</div>;
  }

  const total = Object.values(data).reduce((s, v) => s + v, 0);
  const colors = {
    balanced: '#3b82f6',
    fast: '#22c55e',
    cheap: '#f59e0b',
    best: '#a855f7',
    manual: '#6b7280',
  };

  return (
    <div>
      {/* Bar */}
      <div style={{ display: 'flex', height: 8, borderRadius: 4, overflow: 'hidden', marginBottom: 12 }}>
        {Object.entries(data).map(([strat, count]) => (
          <div
            key={strat}
            style={{
              width: `${(count / total) * 100}%`,
              background: colors[strat] || '#6b7280',
              minWidth: 3,
            }}
          />
        ))}
      </div>
      {/* Legend */}
      <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
        {Object.entries(data).map(([strat, count]) => (
          <div key={strat} style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <div style={{ width: 8, height: 8, borderRadius: 2, background: colors[strat] || '#6b7280' }} />
            <span style={{ fontSize: 12, color: 'var(--text-secondary)' }}>{strat}</span>
            <span style={{ fontSize: 12, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)' }}>{count}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function ProviderBreakdown({ data }) {
  if (!data || Object.keys(data).length === 0) return null;
  const total = Object.values(data).reduce((s, v) => s + v, 0);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      {Object.entries(data)
        .sort((a, b) => b[1] - a[1])
        .map(([prov, count]) => (
          <div key={prov} style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <div style={{ width: 100, fontSize: 13, color: 'var(--text-secondary)' }}>{prov}</div>
            <div style={{ flex: 1, height: 6, background: 'var(--bg-2)', borderRadius: 3, overflow: 'hidden' }}>
              <div style={{ width: `${(count / total) * 100}%`, height: '100%', background: 'var(--accent)', borderRadius: 3 }} />
            </div>
            <div style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-tertiary)', minWidth: 40, textAlign: 'right' }}>
              {Math.round((count / total) * 100)}%
            </div>
          </div>
        ))}
    </div>
  );
}

function Badge({ type, children }) {
  const colors = {
    hit: { bg: 'rgba(34,197,94,0.15)', text: '#22c55e' },
    break: { bg: 'rgba(239,68,68,0.15)', text: '#ef4444' },
    bridge: { bg: 'rgba(168,85,247,0.15)', text: '#a855f7' },
    default: { bg: 'rgba(107,114,128,0.15)', text: '#6b7280' },
  };
  const c = colors[type] || colors.default;
  return (
    <span style={{
      display: 'inline-block',
      padding: '2px 8px',
      borderRadius: 4,
      fontSize: 11,
      fontWeight: 500,
      background: c.bg,
      color: c.text,
    }}>
      {children}
    </span>
  );
}

export function RoutingPage() {
  useEffect(() => {
    loading.value = true;
    Promise.all([
      getRoutingSummary().catch(() => null),
      getRoutingLog().catch(() => []),
    ]).then(([s, log]) => {
      summary.value = s;
      if (Array.isArray(log)) logEntries.value = log;
      loading.value = false;
    });
  }, []);

  if (loading.value) {
    return (
      <div style={{ padding: 'var(--space-2xl)', maxWidth: 1060, width: '100%' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Routing</h1>
        <div style={{ color: 'var(--text-tertiary)' }}>Loading...</div>
      </div>
    );
  }

  const s = summary.value?.summary || {};
  const activeSessions = summary.value?.active_sessions || 0;
  const memBytes = summary.value?.memory_usage_bytes || 0;
  const memMB = (memBytes / 1024 / 1024).toFixed(1);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 1060, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Routing</h1>

      {/* Stats row */}
      <div style={{
        display: 'flex', gap: 'var(--space-xl)', marginBottom: 'var(--space-xl)',
        padding: 'var(--space-lg)', background: 'var(--bg-1)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)',
        flexWrap: 'wrap',
      }}>
        <StatCard label="Routing Decisions" value={s.total_decisions || 0} />
        <StatCard
          label="Affinity Hit Rate"
          value={s.total_decisions ? `${Math.round((s.affinity_hit_rate || 0) * 100)}%` : '--'}
          color={s.affinity_hit_rate > 0.7 ? '#22c55e' : s.affinity_hit_rate > 0.4 ? '#f59e0b' : 'var(--text-primary)'}
        />
        <StatCard label="Bridge Injections" value={s.bridge_injections || 0} />
        <StatCard label="Active Sessions" value={activeSessions} sub={`${memMB} MB memory`} />
      </div>

      {/* Strategy distribution */}
      <div style={{
        marginBottom: 'var(--space-xl)',
        padding: 'var(--space-lg)', background: 'var(--bg-1)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)',
      }}>
        <h2 style={{ fontSize: 14, fontWeight: 600, marginBottom: 'var(--space-md)', color: 'var(--text-secondary)' }}>Strategy Distribution</h2>
        <StrategyBar data={s.by_strategy} />
      </div>

      {/* Provider routing distribution */}
      <div style={{
        marginBottom: 'var(--space-xl)',
        padding: 'var(--space-lg)', background: 'var(--bg-1)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)',
      }}>
        <h2 style={{ fontSize: 14, fontWeight: 600, marginBottom: 'var(--space-md)', color: 'var(--text-secondary)' }}>Provider Distribution</h2>
        <ProviderBreakdown data={s.by_provider} />
      </div>

      {/* Recent routing decisions */}
      <div style={{
        background: 'var(--bg-1)',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)',
        overflow: 'hidden',
      }}>
        <div style={{ padding: '12px 16px', borderBottom: '1px solid var(--border)' }}>
          <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)' }}>Recent Decisions</h2>
        </div>
        {logEntries.value.length === 0 ? (
          <div style={{ padding: 32, textAlign: 'center', color: 'var(--text-tertiary)' }}>
            No routing decisions yet. Use <code style={{ fontFamily: 'var(--font-mono)', background: 'var(--bg-2)', padding: '2px 6px', borderRadius: 3 }}>auto</code> as
            the model name to enable smart routing.
          </div>
        ) : (
          <table>
            <thead>
              <tr style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                <th style={{ padding: '8px 16px', fontWeight: 500, color: 'var(--text-tertiary)' }}>Time</th>
                <th style={{ padding: '8px 16px', fontWeight: 500, color: 'var(--text-tertiary)' }}>Strategy</th>
                <th style={{ padding: '8px 16px', fontWeight: 500, color: 'var(--text-tertiary)' }}>Routed To</th>
                <th style={{ padding: '8px 16px', fontWeight: 500, color: 'var(--text-tertiary)' }}>Affinity</th>
                <th style={{ padding: '8px 16px', fontWeight: 500, color: 'var(--text-tertiary)', textAlign: 'right' }}>Candidates</th>
                <th style={{ padding: '8px 16px', fontWeight: 500, color: 'var(--text-tertiary)', textAlign: 'right' }}>Latency</th>
              </tr>
            </thead>
            <tbody>
              {logEntries.value.map((entry, i) => (
                <tr key={entry.id || i} style={{ borderTop: '1px solid var(--border)' }}>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, color: 'var(--text-tertiary)' }}>
                    {entry.created_at || ''}
                  </td>
                  <td style={{ padding: '10px 16px', fontSize: 12 }}>
                    <Badge type="default">{entry.strategy}</Badge>
                  </td>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    {entry.provider}/{entry.model}
                  </td>
                  <td style={{ padding: '10px 16px' }}>
                    {entry.affinity_hit && <Badge type="hit">hit</Badge>}
                    {entry.affinity_break && <Badge type="break">break</Badge>}
                    {entry.bridge_injected && <Badge type="bridge">bridge</Badge>}
                    {!entry.affinity_hit && !entry.affinity_break && <Badge type="default">new</Badge>}
                  </td>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>
                    {entry.candidates}{entry.filtered > 0 && <span style={{ color: 'var(--text-tertiary)' }}> (-{entry.filtered})</span>}
                  </td>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>
                    {entry.latency_ms}ms
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
