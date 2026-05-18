// CostSummary — two-line view of usage.summary.
//
//   API cost:               $X.XXXX
//   Subscription savings:   $Y.YYYY
//
// `subscription_savings` is computed at query time using current pricing
// against historical token counts (AC48), so it shifts when pricing
// changes — the tooltip explains that.
//
// Post-review minor #2: uses shared fmtCost from utils/format so the
// StatCard and the table cell render identical values for the same
// input (replaces the old local `fmt` helper with subtly different
// thresholds).

import { fmtCost } from '../utils/format';

// hasSubConn (optional) — when explicitly false, the "Subscription
// savings" block is hidden entirely (apikey-only users don't see a
// perpetual $0.0000 nag). When true OR omitted (backward compat),
// the block always renders, INCLUDING when current-window savings
// is 0 — surfaces the concept once a subscription connection exists.
// AC-D1 of 20260515-cost-savings-display (revised per plan-review m7).
export function CostSummary({ summary, hasSubConn }) {
  if (!summary) return null;
  const apiCost = summary.by_cost_source?.apikey?.cost ?? summary.total_cost ?? 0;
  const savings = summary.subscription_savings ?? 0;
  const showSavings = hasSubConn !== false;
  return (
    <div style={{ display: 'flex', gap: 'var(--space-xl)' }}>
      <div>
        <div style={{
          fontSize: 11, color: 'var(--text-tertiary)',
          textTransform: 'uppercase', letterSpacing: '0.05em',
        }}>
          API cost
        </div>
        <div style={{
          fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600,
          color: 'var(--accent)',
        }}>
          {fmtCost(apiCost)}
        </div>
      </div>
      {showSavings && (
        <div
          title={
            'What your subscription connections would have cost at current API rates ' +
            '(price-table changes retroactively shift this number).'
          }
        >
          <div style={{
            fontSize: 11, color: 'var(--text-tertiary)',
            textTransform: 'uppercase', letterSpacing: '0.05em',
            display: 'flex', alignItems: 'center', gap: 4,
          }}>
            Subscription savings
            <span style={{
              fontSize: 9, opacity: 0.6,
              border: '1px solid var(--text-tertiary)',
              borderRadius: '50%', width: 12, height: 12, lineHeight: '11px',
              textAlign: 'center', cursor: 'help',
            }}>
              ?
            </span>
          </div>
          <div style={{
            fontSize: 20, fontFamily: 'var(--font-mono)', fontWeight: 600,
            color: 'var(--status-green)',
          }}>
            {fmtCost(savings)}
          </div>
        </div>
      )}
    </div>
  );
}
