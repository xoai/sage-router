import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { useLocation } from 'wouter-preact';
import { StatusDot } from '../components/status-dot';
import { getStatus, getUsage, getUsageSummary, getConnections, getRoutingSummary } from '../api/client';

const stats = signal(null);
const recentRequests = signal([]);
const sparklineData = signal([]);
const connections = signal([]);
const routingStats = signal(null);
const loading = signal(true);

function StatCard({ label, value, sub, color }) {
  return (
    <div style={{
      background: 'var(--bg-1)',
      border: '1px solid var(--border)',
      borderRadius: 'var(--radius-lg)',
      padding: 'var(--space-lg)',
      flex: '1 1 0',
      minWidth: 140,
    }}>
      <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginBottom: 6, textTransform: 'uppercase', letterSpacing: '0.05em' }}>
        {label}
      </div>
      <div style={{ fontSize: 28, fontWeight: 600, fontFamily: 'var(--font-mono)', color: color || 'var(--text-primary)', lineHeight: 1 }}>
        {value}
      </div>
      {sub && <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 4 }}>{sub}</div>}
    </div>
  );
}

function Sparkline({ data, width = 200, height = 40 }) {
  if (!data || data.length < 2) return null;
  const max = Math.max(...data);
  const min = Math.min(...data);
  const range = max - min || 1;
  const step = width / (data.length - 1);
  const points = data.map((v, i) => `${i * step},${height - ((v - min) / range) * height}`).join(' ');
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} style={{ overflow: 'visible' }}>
      <polyline
        points={points}
        fill="none"
        stroke="var(--accent)"
        stroke-width="1.5"
        stroke-linecap="round"
        stroke-linejoin="round"
      />
    </svg>
  );
}

function formatTokens(n) {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return String(n);
}

function formatCost(n) {
  if (n >= 1) return '$' + n.toFixed(2);
  if (n >= 0.01) return '$' + n.toFixed(3);
  return '$' + n.toFixed(4);
}

function timeAgo(ts) {
  const diff = (Date.now() - new Date(ts).getTime()) / 1000;
  if (diff < 60) return Math.round(diff) + 's ago';
  if (diff < 3600) return Math.round(diff / 60) + 'm ago';
  if (diff < 86400) return Math.round(diff / 3600) + 'h ago';
  return Math.round(diff / 86400) + 'd ago';
}

function formatLatency(ns) {
  if (typeof ns === 'string') return ns;
  const ms = ns / 1e6;
  if (ms < 1000) return Math.round(ms) + 'ms';
  return (ms / 1000).toFixed(1) + 's';
}

function SetupGuide({ step, conns }) {
  const [, setLocation] = useLocation();
  const hasProviders = conns.length > 0;
  const hasRequests = recentRequests.value.length > 0;

  if (hasProviders && hasRequests) return null;

  const steps = [
    {
      num: 1,
      label: 'Add a provider',
      desc: 'Connect to Anthropic, OpenAI, or Google',
      done: hasProviders,
      action: () => setLocation('/providers'),
      actionLabel: 'Add Provider',
    },
    {
      num: 2,
      label: 'Get your connection details',
      desc: 'Copy endpoint URL and API key',
      done: hasProviders,
      action: () => setLocation('/connect'),
      actionLabel: 'Go to Connect',
    },
    {
      num: 3,
      label: 'Send your first request',
      desc: 'Point Claude Code, Cursor, or any OpenAI-compatible tool at Sage Router',
      done: hasRequests,
      action: null,
      actionLabel: null,
    },
  ];

  return (
    <div style={{
      background: 'var(--bg-1)',
      border: '1px solid var(--accent)',
      borderRadius: 'var(--radius-lg)',
      padding: 'var(--space-xl)',
      marginBottom: 'var(--space-xl)',
    }}>
      <div style={{ fontSize: 15, fontWeight: 600, marginBottom: 4 }}>Get started</div>
      <div style={{ fontSize: 13, color: 'var(--text-tertiary)', marginBottom: 'var(--space-lg)' }}>
        Set up Sage Router in 3 steps
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {steps.map(s => (
          <div key={s.num} style={{
            display: 'flex', alignItems: 'center', gap: 12,
            opacity: s.done ? 0.5 : 1,
          }}>
            <div style={{
              width: 24, height: 24, borderRadius: 12,
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              fontSize: 12, fontWeight: 600, fontFamily: 'var(--font-mono)',
              background: s.done ? 'rgba(34,197,94,0.2)' : 'var(--bg-2)',
              color: s.done ? '#22c55e' : 'var(--text-secondary)',
              flexShrink: 0,
            }}>
              {s.done ? '\u2713' : s.num}
            </div>
            <div style={{ flex: 1 }}>
              <div style={{ fontSize: 13, fontWeight: 500, textDecoration: s.done ? 'line-through' : 'none' }}>{s.label}</div>
              <div style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>{s.desc}</div>
            </div>
            {s.action && !s.done && (
              <button
                onClick={s.action}
                style={{
                  padding: '5px 12px', fontSize: 12, fontWeight: 500,
                  background: 'var(--accent)', color: '#fff',
                  borderRadius: 'var(--radius-md)', cursor: 'pointer',
                }}
              >
                {s.actionLabel}
              </button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

function ProviderSummaryRow({ data }) {
  if (!data || Object.keys(data).length === 0) return null;

  return (
    <div style={{
      display: 'flex', gap: 12, marginBottom: 'var(--space-xl)', flexWrap: 'wrap',
    }}>
      {Object.entries(data).map(([prov, info]) => (
        <div key={prov} style={{
          background: 'var(--bg-1)', border: '1px solid var(--border)',
          borderRadius: 'var(--radius-lg)', padding: '12px 16px',
          flex: '1 1 0', minWidth: 160,
        }}>
          <div style={{ fontSize: 13, fontWeight: 500, marginBottom: 4 }}>{prov}</div>
          <div style={{ display: 'flex', gap: 16 }}>
            <div>
              <div style={{ fontSize: 10, color: 'var(--text-tertiary)', textTransform: 'uppercase' }}>Requests</div>
              <div style={{ fontSize: 16, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{info.requests}</div>
            </div>
            <div>
              <div style={{ fontSize: 10, color: 'var(--text-tertiary)', textTransform: 'uppercase' }}>Tokens</div>
              <div style={{ fontSize: 16, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{formatTokens(info.tokens)}</div>
            </div>
            <div>
              <div style={{ fontSize: 10, color: 'var(--text-tertiary)', textTransform: 'uppercase' }}>Cost</div>
              <div style={{ fontSize: 16, fontFamily: 'var(--font-mono)', fontWeight: 600, color: 'var(--accent)' }}>{formatCost(info.cost)}</div>
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}

export function OverviewPage() {
  useEffect(() => {
    loading.value = true;
    Promise.all([
      getStatus().catch(() => ({ active: 0, cooldown: 0, errored: 0 })),
      getUsageSummary().catch(() => ({ total_requests: 0, total_tokens: 0, total_cost: 0, by_provider: {} })),
      getUsage({ limit: 20 }).catch(() => []),
      getConnections().catch(() => []),
      getRoutingSummary().catch(() => null),
    ]).then(([status, summary, usage, conns, routing]) => {
      stats.value = {
        active: status.active || 0,
        cooldown: status.cooldown || 0,
        error: status.errored || 0,
        totalConnections: status.total || 0,
        totalRequests: summary.total_requests || 0,
        totalTokens: summary.total_tokens || 0,
        totalCost: summary.total_cost || 0,
        byProvider: summary.by_provider || {},
      };

      if (Array.isArray(usage)) {
        recentRequests.value = usage.map((r, i) => ({
          id: r.id || i,
          time: timeAgo(r.created_at),
          model: r.model,
          provider: r.provider,
          tokens: r.total_tokens || (r.input_tokens + r.output_tokens),
          latency: formatLatency(r.latency),
          cost: r.cost || 0,
          status: r.status === 'ok' || r.status === 'success' ? 'ok' : r.status || 'ok',
        }));

        // Build sparkline from usage timestamps (bucket into 12 slots over last hour)
        const now = Date.now();
        const hourAgo = now - 3600000;
        const buckets = new Array(12).fill(0);
        usage.forEach(r => {
          const t = new Date(r.created_at).getTime();
          if (t >= hourAgo) {
            const bucket = Math.min(11, Math.floor((t - hourAgo) / 300000)); // 5-min buckets
            buckets[bucket]++;
          }
        });
        if (buckets.some(b => b > 0)) {
          sparklineData.value = buckets;
        }
      }

      connections.value = Array.isArray(conns) ? conns : [];
      routingStats.value = routing;
      loading.value = false;
    });
  }, []);

  if (loading.value) {
    return (
      <div style={{ padding: 'var(--space-2xl)', maxWidth: 960, width: '100%' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Overview</h1>
        <div style={{ color: 'var(--text-tertiary)' }}>Loading...</div>
      </div>
    );
  }

  const s = stats.value || {};

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 960, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Overview</h1>

      {/* Setup guide (shown until first request) */}
      <SetupGuide conns={connections.value} />

      {/* Stat cards */}
      <div style={{ display: 'flex', gap: 'var(--space-md)', flexWrap: 'wrap', marginBottom: 'var(--space-xl)' }}>
        <StatCard
          label="Connections"
          value={s.active || 0}
          sub={s.totalConnections > 0 ? `${s.totalConnections} total` : null}
          color="var(--status-green)"
        />
        <StatCard label="Requests" value={(s.totalRequests || 0).toLocaleString()} />
        <StatCard label="Tokens" value={formatTokens(s.totalTokens || 0)} />
        <StatCard label="Cost" value={formatCost(s.totalCost || 0)} color="var(--accent)" />
      </div>

      {/* Per-provider breakdown */}
      <ProviderSummaryRow data={s.byProvider} />

      {/* Sparkline + routing stats */}
      <div style={{ display: 'flex', gap: 'var(--space-md)', marginBottom: 'var(--space-xl)', flexWrap: 'wrap' }}>
        {/* Sparkline */}
        <div style={{
          background: 'var(--bg-1)',
          border: '1px solid var(--border)',
          borderRadius: 'var(--radius-lg)',
          padding: 'var(--space-lg)',
          flex: '2 1 400px',
          minHeight: 100,
        }}>
          <div style={{ fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 'var(--space-md)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
            Requests (last hour)
          </div>
          {sparklineData.value.length > 0 ? (
            <Sparkline data={sparklineData.value} width={500} height={60} />
          ) : (
            <div style={{ color: 'var(--text-tertiary)', fontSize: 12, paddingTop: 12 }}>
              No recent activity
            </div>
          )}
        </div>

        {/* Routing quick stats */}
        {routingStats.value?.summary && (
          <div style={{
            background: 'var(--bg-1)',
            border: '1px solid var(--border)',
            borderRadius: 'var(--radius-lg)',
            padding: 'var(--space-lg)',
            flex: '1 1 200px',
          }}>
            <div style={{ fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 'var(--space-md)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
              Smart Routing
            </div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              <div>
                <div style={{ fontSize: 10, color: 'var(--text-tertiary)', textTransform: 'uppercase' }}>Decisions</div>
                <div style={{ fontSize: 18, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{routingStats.value.summary.total_decisions || 0}</div>
              </div>
              <div>
                <div style={{ fontSize: 10, color: 'var(--text-tertiary)', textTransform: 'uppercase' }}>Affinity Hit Rate</div>
                <div style={{ fontSize: 18, fontFamily: 'var(--font-mono)', fontWeight: 600, color: '#22c55e' }}>
                  {routingStats.value.summary.total_decisions ? `${Math.round((routingStats.value.summary.affinity_hit_rate || 0) * 100)}%` : '--'}
                </div>
              </div>
              <div>
                <div style={{ fontSize: 10, color: 'var(--text-tertiary)', textTransform: 'uppercase' }}>Active Sessions</div>
                <div style={{ fontSize: 18, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{routingStats.value.active_sessions || 0}</div>
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Recent requests table */}
      <div style={{
        background: 'var(--bg-1)',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)',
        overflow: 'hidden',
      }}>
        <div style={{ padding: 'var(--space-lg)', borderBottom: '1px solid var(--border)', fontSize: 12, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
          Recent Requests
        </div>
        <table>
          <thead>
            <tr style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
              <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Time</th>
              <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Model</th>
              <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Provider</th>
              <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Tokens</th>
              <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Cost</th>
              <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Latency</th>
              <th style={{ padding: '8px 16px', textAlign: 'center', fontWeight: 500 }}>Status</th>
            </tr>
          </thead>
          <tbody>
            {recentRequests.value.length === 0 ? (
              <tr>
                <td colSpan={7} style={{ padding: '32px 16px', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
                  No requests yet. Connect a tool and start sending requests.
                </td>
              </tr>
            ) : recentRequests.value.map(r => (
              <tr key={r.id} style={{ borderTop: '1px solid var(--border)' }}>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-tertiary)' }}>{r.time}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12 }}>{r.model}</td>
                <td style={{ padding: '10px 16px', fontSize: 13 }}>{r.provider}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{r.tokens.toLocaleString()}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right', color: 'var(--accent)' }}>{formatCost(r.cost)}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{r.latency}</td>
                <td style={{ padding: '10px 16px', textAlign: 'center' }}><StatusDot status={r.status} pulse={r.status === 'cooldown'} /></td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
