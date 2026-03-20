import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { getUsage } from '../api/client';

const usageData = signal([]);

const sortField = signal('timestamp');
const sortDir = signal('desc');

function formatTokens(n) {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return n.toString();
}

function SortHeader({ field, children, align = 'left' }) {
  const active = sortField.value === field;
  return (
    <th
      onClick={() => {
        if (active) {
          sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc';
        } else {
          sortField.value = field;
          sortDir.value = 'desc';
        }
      }}
      style={{
        padding: '8px 16px',
        textAlign: align,
        fontWeight: 500,
        cursor: 'pointer',
        userSelect: 'none',
        color: active ? 'var(--text-primary)' : 'var(--text-tertiary)',
      }}
    >
      {children}
      {active && (
        <span style={{ marginLeft: 4, fontSize: 10 }}>
          {sortDir.value === 'asc' ? '\u2191' : '\u2193'}
        </span>
      )}
    </th>
  );
}

function formatLatency(ns) {
  if (typeof ns === 'string') return ns;
  const ms = ns / 1e6;
  if (ms < 1000) return Math.round(ms) + 'ms';
  return (ms / 1000).toFixed(1) + 's';
}

export function UsagePage() {
  useEffect(() => {
    getUsage({ limit: 100 }).then(data => {
      if (Array.isArray(data)) {
        usageData.value = data.map((r, i) => ({
          id: r.id || i,
          timestamp: r.created_at || '',
          provider: r.provider,
          model: r.model,
          inputTokens: r.input_tokens || 0,
          outputTokens: r.output_tokens || 0,
          cost: r.cost || 0,
          latency: formatLatency(r.latency),
        }));
      }
    }).catch(() => {});
  }, []);

  const sorted = [...usageData.value].sort((a, b) => {
    const f = sortField.value;
    const dir = sortDir.value === 'asc' ? 1 : -1;
    if (a[f] < b[f]) return -1 * dir;
    if (a[f] > b[f]) return 1 * dir;
    return 0;
  });

  const totalCost = usageData.value.reduce((s, r) => s + r.cost, 0);
  const totalInput = usageData.value.reduce((s, r) => s + r.inputTokens, 0);
  const totalOutput = usageData.value.reduce((s, r) => s + r.outputTokens, 0);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 1060, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Usage</h1>

      {/* Summary bar */}
      <div style={{
        display: 'flex', gap: 'var(--space-xl)', marginBottom: 'var(--space-xl)',
        padding: 'var(--space-lg)', background: 'var(--bg-1)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)',
      }}>
        <div>
          <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>Total Cost</div>
          <div style={{ fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600, color: 'var(--accent)' }}>${totalCost.toFixed(4)}</div>
        </div>
        <div>
          <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>Input Tokens</div>
          <div style={{ fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{formatTokens(totalInput)}</div>
        </div>
        <div>
          <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>Output Tokens</div>
          <div style={{ fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{formatTokens(totalOutput)}</div>
        </div>
        <div>
          <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>Requests</div>
          <div style={{ fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>{usageData.value.length}</div>
        </div>
      </div>

      {/* Usage table */}
      <div style={{
        background: 'var(--bg-1)',
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)',
        overflow: 'hidden',
      }}>
        <table>
          <thead>
            <tr style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.05em' }}>
              <SortHeader field="timestamp">Time</SortHeader>
              <SortHeader field="provider">Provider</SortHeader>
              <SortHeader field="model">Model</SortHeader>
              <SortHeader field="inputTokens" align="right">Input</SortHeader>
              <SortHeader field="outputTokens" align="right">Output</SortHeader>
              <SortHeader field="cost" align="right">Cost</SortHeader>
              <SortHeader field="latency" align="right">Latency</SortHeader>
            </tr>
          </thead>
          <tbody>
            {sorted.map(r => (
              <tr key={r.id} style={{ borderTop: '1px solid var(--border)' }}>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, color: 'var(--text-tertiary)' }}>{r.timestamp}</td>
                <td style={{ padding: '10px 16px', fontSize: 13 }}>{r.provider}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12 }}>{r.model}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{r.inputTokens.toLocaleString()}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{r.outputTokens.toLocaleString()}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right', color: 'var(--accent)' }}>${r.cost.toFixed(4)}</td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{r.latency}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
