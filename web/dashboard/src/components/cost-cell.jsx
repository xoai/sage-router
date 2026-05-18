// CostCell — shared per-row cost rendering for Recent Requests
// (Overview) and the Usage table.
//
// Apikey rows: a single accent-colored cost value.
// Subscription rows: muted "$0.0000" plus a parenthetical "(≈ $X)"
// showing the would-have-been API cost (estimated_api_cost field
// enriched server-side at routes_api.go's handleGetUsage). Preserves
// the literal $0.00 fact while educating about the avoided cost.
//
// AC-B1, AC-B2, AC-B5 of 20260515-cost-savings-display.
// Post-review minor #2: uses the shared fmtCost helper from utils/format
// so StatCards and table cells render identical values for the same
// input — no per-component threshold drift.
//
// Post-review minor #3: warns once per session if a row has zero
// cost AND non-zero tokens AND cost_source ≠ 'subscription'. Today
// that combination is impossible (apikey rows always carry a real
// cost; subscription rows are gated by cost_source). A future server
// regression that drops the cost_source field would silently render
// such rows as "$0.0000" in accent blue (apikey treatment) — the
// warn surfaces that drift in DevTools.

import { fmtCost } from '../utils/format';

const warned = new Set();

function warnSilentZero(row) {
  const key = `${row.provider}/${row.model}`;
  if (warned.has(key)) return;
  warned.add(key);
  // eslint-disable-next-line no-console
  console.warn(
    `[CostCell] row ${key} has cost=0 with ${row.input_tokens || 0}+${row.output_tokens || 0} tokens ` +
    `but cost_source=${JSON.stringify(row.cost_source ?? null)} ` +
    `— suspected /api/usage response missing cost_source field. ` +
    `Subscription rows should carry cost_source="subscription"; apikey rows should carry a real cost.`
  );
}

export function CostCell({ row }) {
  const isSubscription = row.cost_source === 'subscription';
  const hasUsage = (row.input_tokens || 0) + (row.output_tokens || 0) > 0;
  const cost = row.cost || 0;
  const estimatedAPICost = row.estimated_api_cost || 0;
  const showParenthetical = isSubscription && hasUsage && estimatedAPICost > 0;
  // Defensive: surface a likely-bug shape (cost=0 + tokens>0 + not subscription).
  if (cost === 0 && hasUsage && !isSubscription) {
    warnSilentZero(row);
  }
  return (
    <span style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
      <span
        style={{
          color: isSubscription ? 'var(--text-tertiary)' : 'var(--accent)',
        }}
      >
        {fmtCost(cost)}
      </span>
      {showParenthetical && (
        <span
          style={{
            color: 'var(--text-tertiary)',
            opacity: 0.7,
            marginLeft: 6,
            fontSize: 11,
          }}
          title={`Would have cost ${fmtCost(estimatedAPICost)} at current API rates`}
        >
          (≈ {fmtCost(estimatedAPICost)})
        </span>
      )}
    </span>
  );
}
