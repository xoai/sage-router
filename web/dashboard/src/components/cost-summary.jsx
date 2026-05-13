// CostSummary — two-line view of usage.summary.
//
//   API cost:               $X.XXXX
//   Subscription savings:   $Y.YYYY
//
// `subscription_savings` is computed at query time using current pricing
// against historical token counts (AC48), so it shifts when pricing
// changes — the tooltip explains that.

function fmt(n) {
  const v = typeof n === 'number' ? n : 0;
  if (v >= 100) return '$' + v.toFixed(2);
  return '$' + v.toFixed(4);
}

export function CostSummary({ summary }) {
  if (!summary) return null;
  const apiCost = summary.by_cost_source?.apikey?.cost ?? summary.total_cost ?? 0;
  const savings = summary.subscription_savings ?? 0;
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
          {fmt(apiCost)}
        </div>
      </div>
      {savings > 0 && (
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
            {fmt(savings)}
          </div>
        </div>
      )}
    </div>
  );
}
