import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { getUsage, getKeys } from '../api/client';

const usageData = signal([]);
const allKeys = signal([]);
const selectedKeyFilter = signal('');

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

function loadUsage() {
  const params = { limit: 200 };
  if (selectedKeyFilter.value) {
    params.api_key_id = selectedKeyFilter.value;
  }
  getUsage(params).then(data => {
    if (Array.isArray(data)) {
      usageData.value = data.map((r, i) => ({
        id: r.id || i,
        timestamp: r.created_at || '',
        provider: r.provider,
        model: r.model,
        apiKeyId: r.api_key_id || '',
        inputTokens: r.input_tokens || 0,
        outputTokens: r.output_tokens || 0,
        cost: r.cost || 0,
        latency: formatLatency(r.latency),
      }));
    }
  }).catch(() => {});
}

export function UsagePage() {
  useEffect(() => {
    getKeys().then(data => {
      if (Array.isArray(data)) allKeys.value = data;
    }).catch(() => {});
    loadUsage();
  }, []);

  // Reload when filter changes
  useEffect(() => {
    loadUsage();
  }, [selectedKeyFilter.value]);

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

  // Per-key breakdown (only when viewing all keys)
  const keyBreakdown = !selectedKeyFilter.value && allKeys.value.length > 1
    ? allKeys.value.map(k => {
        const keyUsage = usageData.value.filter(r => r.apiKeyId === k.id);
        return {
          ...k,
          requests: keyUsage.length,
          cost: keyUsage.reduce((s, r) => s + r.cost, 0),
          tokens: keyUsage.reduce((s, r) => s + r.inputTokens + r.outputTokens, 0),
        };
      }).filter(k => k.requests > 0)
    : [];

  // Budget info for selected key
  const selectedKeyInfo = selectedKeyFilter.value
    ? allKeys.value.find(k => k.id === selectedKeyFilter.value)
    : null;

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 1060, width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-xl)' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600 }}>Usage</h1>

        {/* Key filter dropdown */}
        {allKeys.value.length > 0 && (
          <select
            value={selectedKeyFilter.value}
            onChange={e => { selectedKeyFilter.value = e.target.value; }}
            style={{
              padding: '6px 10px', background: 'var(--bg-2)',
              border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
              color: 'var(--text-primary)', fontSize: 12,
            }}
          >
            <option value="">All Keys</option>
            {allKeys.value.map(k => (
              <option key={k.id} value={k.id}>{k.name} ({k.prefix})</option>
            ))}
          </select>
        )}
      </div>

      {/* Summary bar */}
      <div style={{
        display: 'flex', gap: 'var(--space-xl)', marginBottom: 'var(--space-xl)',
        padding: 'var(--space-lg)', background: 'var(--bg-1)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)',
        flexWrap: 'wrap',
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
        {selectedKeyInfo && selectedKeyInfo.budget_monthly > 0 && (
          <div style={{ marginLeft: 'auto' }}>
            <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>Budget</div>
            <div style={{ fontSize: 16, fontFamily: 'var(--font-mono)', fontWeight: 600 }}>
              <span style={{ color: totalCost > selectedKeyInfo.budget_monthly * 0.8 ? 'var(--status-red)' : 'var(--accent)' }}>
                ${totalCost.toFixed(2)}
              </span>
              <span style={{ color: 'var(--text-tertiary)', fontSize: 13 }}> / ${selectedKeyInfo.budget_monthly.toFixed(2)}</span>
              {selectedKeyInfo.budget_hard_limit && (
                <span style={{ fontSize: 9, background: 'var(--status-red)', color: '#fff', padding: '1px 5px', borderRadius: 8, marginLeft: 6, verticalAlign: 'middle' }}>HARD</span>
              )}
            </div>
            <div style={{
              marginTop: 4, height: 4, background: 'var(--bg-3)', borderRadius: 2, overflow: 'hidden',
            }}>
              <div style={{
                height: '100%', borderRadius: 2,
                width: `${Math.min(100, (totalCost / selectedKeyInfo.budget_monthly) * 100)}%`,
                background: totalCost > selectedKeyInfo.budget_monthly * 0.8 ? 'var(--status-red)' : 'var(--accent)',
              }} />
            </div>
          </div>
        )}
      </div>

      {/* Per-key breakdown table (when viewing all keys and multiple exist) */}
      {keyBreakdown.length > 0 && (
        <div style={{
          background: 'var(--bg-1)', border: '1px solid var(--border)',
          borderRadius: 'var(--radius-lg)', overflow: 'hidden',
          marginBottom: 'var(--space-xl)',
        }}>
          <div style={{ padding: '12px 16px', fontSize: 12, fontWeight: 600, borderBottom: '1px solid var(--border)' }}>
            Per-Key Breakdown
          </div>
          <table>
            <thead>
              <tr style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500, color: 'var(--text-tertiary)' }}>Key</th>
                <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500, color: 'var(--text-tertiary)' }}>Requests</th>
                <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500, color: 'var(--text-tertiary)' }}>Tokens</th>
                <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500, color: 'var(--text-tertiary)' }}>Cost</th>
                <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500, color: 'var(--text-tertiary)' }}>Budget</th>
              </tr>
            </thead>
            <tbody>
              {keyBreakdown.map(k => (
                <tr key={k.id} style={{ borderTop: '1px solid var(--border)', cursor: 'pointer' }}
                  onClick={() => { selectedKeyFilter.value = k.id; }}>
                  <td style={{ padding: '10px 16px' }}>
                    <span style={{ fontSize: 13, fontWeight: 500 }}>{k.name}</span>
                    <code style={{ fontSize: 10, color: 'var(--text-tertiary)', marginLeft: 6 }}>{k.prefix}</code>
                  </td>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{k.requests.toLocaleString()}</td>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{formatTokens(k.tokens)}</td>
                  <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right', color: 'var(--accent)' }}>${k.cost.toFixed(4)}</td>
                  <td style={{ padding: '10px 16px', textAlign: 'right' }}>
                    {k.budget_monthly > 0 ? (
                      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'flex-end', gap: 6 }}>
                        <div style={{ width: 60, height: 4, background: 'var(--bg-3)', borderRadius: 2, overflow: 'hidden' }}>
                          <div style={{
                            height: '100%', borderRadius: 2,
                            width: `${Math.min(100, (k.cost / k.budget_monthly) * 100)}%`,
                            background: k.cost > k.budget_monthly * 0.8 ? 'var(--status-red)' : 'var(--accent)',
                          }} />
                        </div>
                        <span style={{ fontFamily: 'var(--font-mono)', fontSize: 11 }}>
                          {Math.round((k.cost / k.budget_monthly) * 100)}%
                        </span>
                      </div>
                    ) : (
                      <span style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

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
            {sorted.length === 0 && (
              <tr>
                <td colSpan={7} style={{ padding: '24px 16px', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
                  No usage data yet. Connect a tool and make some requests.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
