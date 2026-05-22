import { signal } from '@preact/signals';
import { useEffect, useRef } from 'preact/hooks';
import { getUsage, getUsageSummary, getKeys, getConnections } from '../api/client';
import { CostSummary } from '../components/cost-summary';
import { CostCell } from '../components/cost-cell';
import {
  toUTCStartOfDay,
  toUTCEndOfDay,
  presetDateStrings,
  shouldShowBreakdown,
  shouldShowBudgetBar,
  singleSelectedKey,
} from './usage-helpers';

const usageData = signal([]);
const usageSummary = signal(null);
const allKeys = signal([]);
const connections = signal([]);

// Multi-key selection (cycle 20260517-usage-page-filters T9a). Empty
// Set = all keys. Mutations must assign a brand-new Set instance —
// Preact signals are identity-based, so `selectedKeys.value.add(x)`
// is a silent no-op.
const selectedKeys = signal(new Set());
const keySearch = signal('');
const keyPopoverOpen = signal(false);

// Date range filter. Plain "YYYY-MM-DD" strings — the values from
// <input type="date">. Empty string = no bound on that side.
// Transformation to ISO-Z happens at fetch time via toUTC*OfDay
// helpers (signal-string identity means same-string reassignment
// does NOT refire; see usage-wire.test.js).
const fromDate = signal('');
const toDate = signal('');

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
          {sortDir.value === 'asc' ? '↑' : '↓'}
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
  // Date range: transform local "YYYY-MM-DD" → UTC ISO-Z byte-for-byte
  // matching the server's timeStr storage. toUTC*OfDay returns "" for
  // empty inputs; buildQS skips empty strings.
  const range = {};
  if (fromDate.value) range.from = toUTCStartOfDay(fromDate.value);
  if (toDate.value) range.to = toUTCEndOfDay(toDate.value);

  // Multi-key: Set → Array at the callsite. buildQS serializes the
  // array as repeated params (api_key_id=A&api_key_id=B). Empty Set
  // yields empty array which buildQS skips entirely.
  const keyIds = Array.from(selectedKeys.value);

  const params = { limit: 200, ...range };
  if (keyIds.length > 0) params.api_key_id = keyIds;

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
        cacheReadTokens: r.cache_read_tokens || 0,
        cacheWriteTokens: r.cache_write_tokens || 0,
        // M4 compression measurement — savedTokens is shown only when
        // tokensBefore > 0 (i.e. compression actually ran on this request).
        tokensBefore: r.tokens_before || 0,
        savedTokens: Math.max(0, (r.tokens_before || 0) - (r.tokens_after || 0)),
        cost: r.cost || 0,
        cost_source: r.cost_source || 'apikey',
        input_tokens: r.input_tokens || 0,
        output_tokens: r.output_tokens || 0,
        estimated_api_cost: r.estimated_api_cost || 0,
        latency: formatLatency(r.latency),
      }));
    }
  }).catch(() => {});

  // Summary follows the same filter as the row list (cycle
  // 20260517-usage-page-filters AC9c). Previously summary was a
  // whole-system view; the user-facing decision was consistency over
  // purity — selection scopes everything.
  const summaryParams = { ...range };
  if (keyIds.length > 0) summaryParams.api_key_id = keyIds;
  getUsageSummary(summaryParams).then(s => { usageSummary.value = s; }).catch(() => {});
}

export function UsagePage() {
  const popoverRef = useRef(null);

  useEffect(() => {
    getKeys({ limit: 200 }).then(res => {
      if (Array.isArray(res?.items)) allKeys.value = res.items;
    }).catch(() => {});
    getConnections().then(data => {
      if (Array.isArray(data)) connections.value = data;
    }).catch(() => {});
    loadUsage();
  }, []);

  // Refetch when filters change. Signal identity rules:
  //   - selectedKeys: Set → identity-based, mutations create new instance
  //   - fromDate / toDate: strings → value-based, same-string is no-op
  // Pinned by usage-wire.test.js.
  useEffect(() => {
    loadUsage();
  }, [selectedKeys.value, fromDate.value, toDate.value]);

  // Close popover on ESC or click outside.
  useEffect(() => {
    if (!keyPopoverOpen.value) return undefined;
    const onKey = e => { if (e.key === 'Escape') keyPopoverOpen.value = false; };
    const onClick = e => {
      if (popoverRef.current && !popoverRef.current.contains(e.target)) {
        keyPopoverOpen.value = false;
      }
    };
    document.addEventListener('keydown', onKey);
    // Defer adding the click listener so the same click that OPENED
    // the popover (caught at capture phase before this effect runs)
    // doesn't immediately close it. Belt-and-suspenders: the trigger
    // button ALSO calls e.stopPropagation() in its onClick (below)
    // — both guards are intentional. Removing either is safe today;
    // removing BOTH would let toggle-open immediately re-close.
    const t = setTimeout(() => document.addEventListener('click', onClick), 0);
    return () => {
      clearTimeout(t);
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('click', onClick);
    };
  }, [keyPopoverOpen.value]);

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
  const totalCacheRead = usageData.value.reduce((s, r) => s + r.cacheReadTokens, 0);
  const totalCacheWrite = usageData.value.reduce((s, r) => s + r.cacheWriteTokens, 0);
  const cacheHitRate = totalInput > 0 ? (totalCacheRead / totalInput * 100) : 0;

  // Per-Key Breakdown gating (AC9). When 2+ keys selected, only show
  // breakdown for those keys; the server has already filtered the
  // usageData rows, so we only need to project the breakdown across
  // the selected subset of allKeys.
  const showBreakdown = shouldShowBreakdown(selectedKeys.value, allKeys.value);
  const keyBreakdown = showBreakdown
    ? allKeys.value
        .filter(k => selectedKeys.value.size === 0 || selectedKeys.value.has(k.id))
        .map(k => {
          const keyUsage = usageData.value.filter(r => r.apiKeyId === k.id);
          return {
            ...k,
            requests: keyUsage.length,
            cost: keyUsage.reduce((s, r) => s + r.cost, 0),
            tokens: keyUsage.reduce((s, r) => s + r.inputTokens + r.outputTokens, 0),
          };
        })
        .filter(k => k.requests > 0)
    : [];

  // Single-key budget bar (AC9b). singleSelectedKey returns null when
  // size !== 1, so the budget block is naturally omitted.
  const selectedKeyInfo = singleSelectedKey(selectedKeys.value, allKeys.value);
  const showBudget = shouldShowBudgetBar(selectedKeys.value, allKeys.value);

  // Multi-select popover — filtered key list.
  const filteredKeys = allKeys.value.filter(k => {
    if (!keySearch.value) return true;
    const q = keySearch.value.toLowerCase();
    return (k.name || '').toLowerCase().includes(q)
      || (k.prefix || '').toLowerCase().includes(q);
  });

  const toggleKey = (id) => {
    const next = new Set(selectedKeys.value);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    selectedKeys.value = next;
  };
  const clearKeys = () => { selectedKeys.value = new Set(); };
  const selectAllKeys = () => {
    selectedKeys.value = new Set(allKeys.value.map(k => k.id));
  };

  // Trigger label for the multi-select.
  const keyTriggerLabel = selectedKeys.value.size === 0
    ? 'All keys'
    : selectedKeys.value.size === 1
      ? (singleSelectedKey(selectedKeys.value, allKeys.value)?.name || '1 key')
      : `${selectedKeys.value.size} keys`;

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 1060, width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 'var(--space-xl)', gap: 'var(--space-md)', flexWrap: 'wrap' }}>
        <h1 style={{ fontSize: 20, fontWeight: 600 }}>Usage</h1>

        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-sm)', flexWrap: 'wrap', justifyContent: 'flex-end' }}>
          {/* Date range picker (AC1-AC4) */}
          <label style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>From</label>
          <input
            type="date"
            value={fromDate.value}
            onChange={e => { fromDate.value = e.target.value; }}
            style={inputStyle}
          />
          <label style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>To</label>
          <input
            type="date"
            value={toDate.value}
            onChange={e => { toDate.value = e.target.value; }}
            style={inputStyle}
          />
          {/* Preset chips (AC2). Hydrate the date inputs; no active-preset state. */}
          <button style={chipStyle} onClick={() => { const r = presetDateStrings(7); fromDate.value = r.from; toDate.value = r.to; }}>Last 7d</button>
          <button style={chipStyle} onClick={() => { const r = presetDateStrings(30); fromDate.value = r.from; toDate.value = r.to; }}>Last 30d</button>
          <button style={chipStyle} onClick={() => { fromDate.value = ''; toDate.value = ''; }}>All</button>

          {/* Multi-select keys (AC5-AC7) */}
          {allKeys.value.length > 0 && (
            <div style={{ position: 'relative' }} ref={popoverRef}>
              <button
                onClick={e => { e.stopPropagation(); keyPopoverOpen.value = !keyPopoverOpen.value; }}
                style={{ ...inputStyle, cursor: 'pointer', minWidth: 110, textAlign: 'left' }}
              >
                {keyTriggerLabel}
                {selectedKeys.value.size > 1 && (
                  <span style={{ marginLeft: 6, fontSize: 10, background: 'var(--accent)', color: '#fff', padding: '1px 5px', borderRadius: 8 }}>
                    {selectedKeys.value.size}
                  </span>
                )}
                <span style={{ marginLeft: 6, fontSize: 10, color: 'var(--text-tertiary)' }}>▼</span>
              </button>

              {keyPopoverOpen.value && (
                <div style={{
                  position: 'absolute', top: '100%', right: 0, marginTop: 4,
                  minWidth: 260, maxHeight: 320, overflowY: 'auto',
                  background: 'var(--bg-2)', border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)', zIndex: 10,
                  boxShadow: '0 4px 12px rgba(0,0,0,0.2)',
                }}>
                  <div style={{ padding: 8, borderBottom: '1px solid var(--border)' }}>
                    <input
                      type="text"
                      placeholder="Search keys..."
                      value={keySearch.value}
                      onChange={e => { keySearch.value = e.target.value; }}
                      style={{ ...inputStyle, width: '100%' }}
                    />
                  </div>
                  <div style={{ padding: '4px 0' }}>
                    {filteredKeys.length === 0 ? (
                      <div style={{ padding: '12px 16px', fontSize: 12, color: 'var(--text-tertiary)' }}>
                        No keys match
                      </div>
                    ) : (
                      filteredKeys.map(k => {
                        const checked = selectedKeys.value.has(k.id);
                        return (
                          <label
                            key={k.id}
                            style={{
                              display: 'flex', alignItems: 'center', gap: 8,
                              padding: '6px 12px', fontSize: 13, cursor: 'pointer',
                              background: checked ? 'var(--bg-3)' : 'transparent',
                            }}
                          >
                            <input
                              type="checkbox"
                              checked={checked}
                              onChange={() => toggleKey(k.id)}
                            />
                            <span style={{ flex: 1 }}>{k.name}</span>
                            <code style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>{k.prefix}</code>
                          </label>
                        );
                      })
                    )}
                  </div>
                  <div style={{ display: 'flex', gap: 6, padding: 8, borderTop: '1px solid var(--border)' }}>
                    <button style={chipStyle} onClick={clearKeys}>Clear</button>
                    <button style={chipStyle} onClick={selectAllKeys}>Select all</button>
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Summary bar */}
      <div style={{
        display: 'flex', gap: 'var(--space-xl)', marginBottom: 'var(--space-xl)',
        padding: 'var(--space-lg)', background: 'var(--bg-1)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)',
        flexWrap: 'wrap',
      }}>
        <CostSummary
          summary={usageSummary.value}
          hasSubConn={connections.value.some(c => c.auth_type === 'subscription' && c.state !== 'disabled')}
        />
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
        {totalCacheRead > 0 && (
          <div>
            <div style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>Cache Hit Rate</div>
            <div style={{ fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600, color: 'var(--status-green)' }}>{cacheHitRate.toFixed(1)}%</div>
            <div style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>{formatTokens(totalCacheRead)} read / {formatTokens(totalCacheWrite)} write</div>
          </div>
        )}
        {showBudget && selectedKeyInfo && (
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

      {/* Per-key breakdown table */}
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
                  onClick={() => { selectedKeys.value = new Set([k.id]); }}>
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
              <SortHeader field="cacheReadTokens" align="right">Cached</SortHeader>
              <SortHeader field="savedTokens" align="right">
                <span title="Tool-output compression savings. The before-count is a tokenizer estimate; the after-count is provider-actual.">≈ Saved</span>
              </SortHeader>
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
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right', color: r.cacheReadTokens > 0 ? 'var(--status-green)' : 'var(--text-tertiary)' }}>
                  {r.cacheReadTokens > 0 ? formatTokens(r.cacheReadTokens) : '—'}
                </td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right', color: r.tokensBefore > 0 ? 'var(--status-green)' : 'var(--text-tertiary)' }}>
                  {r.tokensBefore > 0 ? '≈ ' + formatTokens(r.savedTokens) : '—'}
                </td>
                <td style={{ padding: '10px 16px', textAlign: 'right' }}><CostCell row={r} /></td>
                <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12, textAlign: 'right' }}>{r.latency}</td>
              </tr>
            ))}
            {sorted.length === 0 && (
              <tr>
                <td colSpan={9} style={{ padding: '24px 16px', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
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

const inputStyle = {
  padding: '6px 10px',
  background: 'var(--bg-2)',
  border: '1px solid var(--border)',
  borderRadius: 'var(--radius-md)',
  color: 'var(--text-primary)',
  fontSize: 12,
};

const chipStyle = {
  padding: '6px 10px',
  background: 'var(--bg-2)',
  border: '1px solid var(--border)',
  borderRadius: 'var(--radius-md)',
  color: 'var(--text-primary)',
  fontSize: 11,
  cursor: 'pointer',
};
