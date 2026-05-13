import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { addToast } from '../components/toast';
import { fmtPrice, fmtTimestamp } from '../utils/format';
import {
  getAliases, setAlias, deleteAlias,
  getCombos, createCombo, deleteCombo,
  getModels,
  getCatalogModels, putCatalogPricing, deleteCatalogPricing,
  getProviders,
} from '../api/client';

const aliases = signal([]);
const combos = signal([]);
const availableModels = signal([]); // /api/models (filtered to active connections)
const catalogModels = signal([]);   // /api/catalog/models (full catalog with source badges)
// providerMeta keyed by provider id; mirrors providers.jsx's signal but
// scoped to this page. Carryover #20 — the Reset confirm copy branches
// on subscription_discoverable, which lives in this map.
const providerMeta = signal({});
const providerFilter = signal('all');

// Inline pricing editor state. editingPricing is "<provider>/<model_id>"
// of the row currently being edited; pricingDraft holds the *raw string*
// form values until the user clicks Save. We store strings, not floats,
// so incremental decimal entry ("0.", "0.0", "0.00") doesn't get
// coerced through parseFloat every keystroke — the M2.11 review's
// MINOR-1 finding. parseFloat is deferred to Save time.
const editingPricing = signal(null);
const pricingDraft = signal({
  input_price: '0',
  output_price: '0',
  cache_read_price: '0',
  cache_write_price: '0',
  thinking_price: '0',
});

function loadAliases() {
  getAliases().then(data => {
    if (data && typeof data === 'object' && !Array.isArray(data)) {
      aliases.value = Object.entries(data).map(([name, target]) => ({ name, target }));
    } else if (Array.isArray(data)) {
      aliases.value = data;
    }
  }).catch(() => {});
}

function loadCombos() {
  getCombos().then(data => {
    if (Array.isArray(data)) {
      combos.value = data;
    }
  }).catch(() => {});
}

function loadModels() {
  getModels().then(data => {
    if (Array.isArray(data)) {
      availableModels.value = data;
    }
  }).catch(() => {});
}

function loadCatalogModels() {
  getCatalogModels().then(data => {
    if (Array.isArray(data)) {
      catalogModels.value = data;
    }
  }).catch(err => {
    // M2.11 review MINOR-3: don't silently swallow catalog fetch
    // errors. An unreachable backend or 500 from the catalog DB
    // should surface to the operator so they can act. 401 is the
    // single exception — the sage:unauthorized event already
    // navigates the user away; an extra toast would flash before
    // the redirect (carryover #25).
    if (err.silent) return;
    addToast('Failed to load catalog: ' + err.message, 'error');
  });
}

function loadProviderMeta() {
  // /api/providers carries the subscription_discoverable flag the
  // Reset confirm copy branches on (carryover #20). Silently no-op
  // on failure: a missing meta map just falls through to the generic
  // confirm copy, which is honest about the fallback behavior.
  getProviders().then(data => {
    if (data && typeof data === 'object' && !Array.isArray(data)) {
      providerMeta.value = data;
    }
  }).catch(() => {});
}

// Models Discovery M2.10 — color-coded source badges so the operator
// can tell at a glance where a row's data came from. Distinct colors
// reuse the existing status palette so the dashboard stays cohesive.
// openrouter uses --status-yellow (defined in tokens.css) to stay
// visibly distinct from discovery's --accent — review M2.11 MAJOR-2
// caught that --status-amber was undefined and silently fell back to
// --accent, merging the two visually.
const sourceBadgeStyle = {
  seed:       { background: 'var(--bg-3)',         color: 'var(--text-tertiary)' },
  discovery:  { background: 'var(--accent-muted)', color: 'var(--accent)' },
  openrouter: { background: 'var(--accent-muted)', color: 'var(--status-yellow)' },
  user:       { background: 'var(--accent)',       color: 'var(--bg-0)' },
};

// fmtDraftValue stringifies a row's numeric price for the editor. We
// keep the raw string in pricingDraft (see MINOR-1 above) so we render
// "0" for legitimate zero — not the empty string — and the user sees
// what they're about to PUT.
function fmtDraftValue(v) {
  if (v == null) return '0';
  return String(v);
}

function startEditPricing(row) {
  // Carryover #22 — pricingDraft is a single global signal, so
  // starting an edit on a second row while one is open silently
  // discards the first row's in-progress draft. Confirm before
  // overwriting. The (modelId !== newModelId) guard skips the prompt
  // when the user re-clicks Edit on the same row.
  const newModelId = row.provider + '/' + row.model_id;
  if (editingPricing.value && editingPricing.value !== newModelId) {
    if (!confirm(`Unsaved pricing changes on ${editingPricing.value} will be lost. Continue?`)) {
      return;
    }
  }
  // Carryover #21 — edit+reload race-safety. loadCatalogModels can
  // fire at any time (toast retry, manual reload). The pricingDraft
  // is keyed by (provider, model_id) — both stable identifiers — so
  // a refresh while the user is mid-edit replaces row.input_price
  // etc. in catalogModels but leaves pricingDraft (and editingPricing)
  // untouched. The row identity is the same on both sides of the
  // refresh, so when Save fires, putCatalogPricing addresses the same
  // (provider, model_id) the user originally clicked. Safe by virtue
  // of stable composite-key identity, not by mutex or version stamp.
  editingPricing.value = newModelId;
  pricingDraft.value = {
    input_price: fmtDraftValue(row.input_price),
    output_price: fmtDraftValue(row.output_price),
    cache_read_price: fmtDraftValue(row.cache_read_price),
    cache_write_price: fmtDraftValue(row.cache_write_price),
    thinking_price: fmtDraftValue(row.thinking_price),
  };
}

function cancelEditPricing() {
  editingPricing.value = null;
}

function updatePricingDraft(field, value) {
  // Store the raw string verbatim. parseFloat happens at savePricing,
  // not here — keeps decimal entry usable (typing "0." doesn't snap
  // back to "0" through truthiness coercion).
  pricingDraft.value = { ...pricingDraft.value, [field]: value };
}

function savePricing(row) {
  // PUT is replace-not-patch (M2.10): all five fields are required.
  // Coerce the raw string drafts to numbers at the boundary — empty
  // string or non-numeric → 0 (the backend accepts 0 as a legitimate
  // free-tier price; see parseORFloat empty-string contract in ADR-3).
  const num = s => {
    const v = parseFloat(s);
    return Number.isFinite(v) ? v : 0;
  };
  const body = {
    input_price: num(pricingDraft.value.input_price),
    output_price: num(pricingDraft.value.output_price),
    cache_read_price: num(pricingDraft.value.cache_read_price),
    cache_write_price: num(pricingDraft.value.cache_write_price),
    thinking_price: num(pricingDraft.value.thinking_price),
  };
  putCatalogPricing(row.provider, row.model_id, body)
    .then(() => {
      addToast(`Pricing updated for ${row.provider}/${row.model_id}`, 'success');
      editingPricing.value = null;
      loadCatalogModels();
    })
    .catch(err => addToast('Failed to save pricing: ' + err.message, 'error'));
}

function resetPricing(row) {
  // DELETE removes the user override; next discovery / openrouter
  // refresh re-populates if one is scheduled. Idempotent. (M2.11
  // review MINOR-2 + carryover #20 — copy branches on the provider's
  // subscription_discoverable flag because a non-discoverable provider
  // has no refresh cycle to re-populate the row, and the operator
  // should know they're committing to a manual re-price.)
  const meta = providerMeta.value[row.provider];
  const subscriptionDiscoverable = meta?.subscription_discoverable;
  let confirmMsg;
  if (subscriptionDiscoverable === false) {
    confirmMsg = `Remove pricing override for ${row.provider}/${row.model_id}?\n\n` +
      `This provider has no automatic refresh cycle. The row will stay at ` +
      `seed pricing (or $0 if none) until you manually re-price it.`;
  } else {
    confirmMsg = `Remove pricing override for ${row.provider}/${row.model_id}?\n\n` +
      `The row will fall back to seed/discovery/OpenRouter pricing if present, ` +
      `or to $0 if no other source has populated this row. Next discovery / ` +
      `OpenRouter refresh may re-populate it.`;
  }
  if (!confirm(confirmMsg)) {
    return;
  }
  deleteCatalogPricing(row.provider, row.model_id)
    .then(() => {
      addToast(`Pricing override removed for ${row.provider}/${row.model_id}`, 'info');
      loadCatalogModels();
    })
    .catch(err => addToast('Failed to remove override: ' + err.message, 'error'));
}

const editingAlias = signal(null);
const editValue = signal('');

// ── Combo modal state ──
const showComboModal = signal(false);
const comboName = signal('');
const comboModels = signal(['']); // array of "provider/model" strings

function addComboEntry() {
  comboModels.value = [...comboModels.value, ''];
}

function removeComboEntry(idx) {
  comboModels.value = comboModels.value.filter((_, i) => i !== idx);
}

function updateComboEntry(idx, val) {
  const updated = [...comboModels.value];
  updated[idx] = val;
  comboModels.value = updated;
}

function moveComboEntry(idx, dir) {
  const arr = [...comboModels.value];
  const target = idx + dir;
  if (target < 0 || target >= arr.length) return;
  [arr[idx], arr[target]] = [arr[target], arr[idx]];
  comboModels.value = arr;
}

function handleSaveCombo() {
  const name = comboName.value.trim();
  const models = comboModels.value.filter(m => m.trim());
  if (!name) { addToast('Combo name is required', 'warning'); return; }
  if (models.length < 2) { addToast('Add at least 2 models for fallback', 'warning'); return; }

  createCombo({ name, models }).then(() => {
    addToast(`Combo "${name}" created`, 'success');
    showComboModal.value = false;
    comboName.value = '';
    comboModels.value = [''];
    loadCombos();
  }).catch(err => {
    addToast('Failed: ' + err.message, 'error');
  });
}

function handleDeleteCombo(id, name) {
  deleteCombo(id).then(() => {
    addToast(`Combo "${name}" deleted`, 'info');
    loadCombos();
  }).catch(err => {
    addToast('Failed: ' + err.message, 'error');
  });
}

// ── Components ──

// PriceInput — narrow numeric input for the inline pricing editor.
// Accepts decimals (incl. tiny per-token prices like 0.0003). The
// onChange callback receives the raw string so the caller can decide
// whether/when to parseFloat. Carryover #23 — previously accepted a
// placeholder prop that never displayed (controlled inputs always have
// a value); dead prop removed.
function PriceInput({ value, onChange }) {
  return (
    <input
      type="text"
      inputMode="decimal"
      value={value}
      onInput={e => onChange(e.target.value)}
      style={{
        width: 70, padding: '4px 6px', background: 'var(--bg-2)',
        border: '1px solid var(--accent)', borderRadius: 'var(--radius-sm)',
        color: 'var(--text-primary)', fontSize: 11, fontFamily: 'var(--font-mono)',
        textAlign: 'right',
      }}
    />
  );
}

function AliasRow({ alias }) {
  const isEditing = editingAlias.value === alias.name;

  const startEdit = () => {
    editingAlias.value = alias.name;
    editValue.value = alias.target;
  };

  const saveEdit = () => {
    setAlias({ name: alias.name, target: editValue.value }).then(() => {
      editingAlias.value = null;
      addToast(`Alias "${alias.name}" updated`, 'success');
      loadAliases();
    }).catch(err => {
      addToast('Failed: ' + err.message, 'error');
    });
  };

  const cancelEdit = () => { editingAlias.value = null; };

  const handleDelete = () => {
    deleteAlias(alias.name).then(() => {
      addToast(`Alias "${alias.name}" deleted`, 'info');
      loadAliases();
    }).catch(err => {
      addToast('Failed: ' + err.message, 'error');
    });
  };

  return (
    <tr style={{ borderTop: '1px solid var(--border)' }}>
      <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 13, color: 'var(--accent)' }}>
        {alias.name}
      </td>
      <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 12 }}>
        {isEditing ? (
          <input
            type="text"
            value={editValue.value}
            onInput={e => { editValue.value = e.target.value; }}
            onKeyDown={e => {
              if (e.key === 'Enter') saveEdit();
              if (e.key === 'Escape') cancelEdit();
            }}
            style={{
              width: '100%', padding: '4px 8px', background: 'var(--bg-2)',
              border: '1px solid var(--accent)', borderRadius: 'var(--radius-sm)',
              color: 'var(--text-primary)', fontSize: 12, fontFamily: 'var(--font-mono)',
            }}
            autoFocus
          />
        ) : (
          <span>{alias.target}</span>
        )}
      </td>
      <td style={{ padding: '10px 16px', textAlign: 'right' }}>
        {isEditing ? (
          <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
            <button onClick={saveEdit} style={{ fontSize: 11, color: 'var(--status-green)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Save</button>
            <button onClick={cancelEdit} style={{ fontSize: 11, color: 'var(--text-tertiary)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Cancel</button>
          </div>
        ) : (
          <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
            <button onClick={startEdit} style={{ fontSize: 11, color: 'var(--text-tertiary)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Edit</button>
            <button onClick={handleDelete} style={{ fontSize: 11, color: 'var(--status-red)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Delete</button>
          </div>
        )}
      </td>
    </tr>
  );
}

function ComboCard({ combo }) {
  return (
    <div style={{
      background: 'var(--bg-1)',
      border: '1px solid var(--border)',
      borderRadius: 'var(--radius-lg)',
      padding: 'var(--space-lg)',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-sm)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ fontFamily: 'var(--font-mono)', fontSize: 13, fontWeight: 500 }}>{combo.name}</span>
          <span style={{
            fontSize: 10, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)',
            background: 'var(--bg-2)', padding: '2px 6px', borderRadius: 'var(--radius-sm)',
          }}>
            fallback
          </span>
        </div>
        <button
          onClick={() => handleDeleteCombo(combo.id, combo.name)}
          style={{ fontSize: 11, color: 'var(--status-red)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}
        >
          Delete
        </button>
      </div>
      <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap', alignItems: 'center' }}>
        {(combo.models || []).map((m, i) => (
          <span key={i} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--text-secondary)',
              background: 'var(--bg-3)', padding: '3px 8px',
              borderRadius: 'var(--radius-sm)',
            }}>
              {m}
            </span>
            {i < (combo.models || []).length - 1 && (
              <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>→</span>
            )}
          </span>
        ))}
      </div>
    </div>
  );
}

// ── Page ──

export function ModelsPage() {
  useEffect(() => {
    loadAliases();
    loadCombos();
    loadModels();
    loadCatalogModels();
    loadProviderMeta();
  }, []);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 1100, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Models</h1>

      {/* Catalog (Models Discovery M2.11) — full catalog rows with
          source badge + inline pricing editor. Distinct from the
          "Available Models" view: this shows EVERY row regardless of
          whether a connection currently exists for it. */}
      {catalogModels.value.length > 0 && (() => {
        const providers = [...new Set(catalogModels.value.map(m => m.provider))];
        const filtered = providerFilter.value === 'all'
          ? catalogModels.value
          : catalogModels.value.filter(m => m.provider === providerFilter.value);
        return (
          <div style={{ marginBottom: 'var(--space-2xl)' }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-md)' }}>
              <div>
                <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)' }}>
                  Model Catalog
                  <span style={{ fontWeight: 400, color: 'var(--text-tertiary)', marginLeft: 6, fontSize: 12 }}>
                    ({filtered.length})
                  </span>
                </h2>
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 2 }}>
                  Live discovery + seed + OpenRouter pricing oracle. Edit a row to override pricing locally.
                </div>
              </div>
              {providers.length > 1 && (
                <select
                  value={providerFilter.value}
                  onChange={e => { providerFilter.value = e.target.value; }}
                  style={{
                    padding: '4px 8px', fontSize: 12, background: 'var(--bg-2)',
                    border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
                    color: 'var(--text-primary)',
                  }}
                >
                  <option value="all">All providers</option>
                  {providers.map(p => <option key={p} value={p}>{p}</option>)}
                </select>
              )}
            </div>
            <div style={{
              background: 'var(--bg-1)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-lg)',
              // Carryover #26 — horizontal scroll on the catalog table.
              // The dashboard's max-width is 1100px but the 8-column
              // catalog table (model + source + 5 prices + actions)
              // can exceed that on a narrow viewport once OpenRouter's
              // qualified IDs land in the Model column (vendor/model
              // strings are wider than bare model IDs). overflow-x:
              // auto keeps the table inside the rounded card frame
              // without scaling its columns down to unreadable widths.
              overflow: 'auto',
            }}>
              <table style={{ width: '100%', minWidth: 720 }}>
                <thead>
                  <tr style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                    <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Model</th>
                    <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Source</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Input</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Output</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Cache R/W</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Updated</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {filtered.map(m => {
                    const key = m.provider + '/' + m.model_id;
                    const editing = editingPricing.value === key;
                    const pricingSrc = m.pricing_source || m.source || 'seed';
                    const badgeStyle = sourceBadgeStyle[pricingSrc] || sourceBadgeStyle.seed;
                    return (
                      <tr key={key} style={{ borderTop: '1px solid var(--border)' }}>
                        <td style={{ padding: '10px 16px' }}>
                          <div style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{m.provider}/{m.model_id}</div>
                          {m.display_name && (
                            <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 2 }}>{m.display_name}</div>
                          )}
                        </td>
                        <td style={{ padding: '10px 16px' }}>
                          <span style={{
                            display: 'inline-block', padding: '2px 8px',
                            fontSize: 10, fontFamily: 'var(--font-mono)',
                            fontWeight: 500, textTransform: 'uppercase', letterSpacing: '0.05em',
                            borderRadius: 'var(--radius-sm)',
                            ...badgeStyle,
                          }}>
                            {pricingSrc}
                          </span>
                        </td>
                        {editing ? (
                          <>
                            <td style={{ padding: '6px 8px' }}>
                              <PriceInput value={pricingDraft.value.input_price} onChange={v => updatePricingDraft('input_price', v)} />
                            </td>
                            <td style={{ padding: '6px 8px' }}>
                              <PriceInput value={pricingDraft.value.output_price} onChange={v => updatePricingDraft('output_price', v)} />
                            </td>
                            <td style={{ padding: '6px 8px' }}>
                              <div style={{ display: 'flex', gap: 4 }}>
                                <PriceInput value={pricingDraft.value.cache_read_price} onChange={v => updatePricingDraft('cache_read_price', v)} />
                                <PriceInput value={pricingDraft.value.cache_write_price} onChange={v => updatePricingDraft('cache_write_price', v)} />
                              </div>
                            </td>
                            <td style={{ padding: '10px 16px', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}>
                              {fmtTimestamp(m.updated_at)}
                            </td>
                            <td style={{ padding: '10px 16px', textAlign: 'right' }}>
                              <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                                <button onClick={() => savePricing(m)} style={{ fontSize: 11, color: 'var(--status-green)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Save</button>
                                <button onClick={cancelEditPricing} style={{ fontSize: 11, color: 'var(--text-tertiary)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Cancel</button>
                              </div>
                            </td>
                          </>
                        ) : (
                          <>
                            <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}>
                              {fmtPrice(m.input_price)}
                            </td>
                            <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}>
                              {fmtPrice(m.output_price)}
                            </td>
                            <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}>
                              {fmtPrice(m.cache_read_price)} / {fmtPrice(m.cache_write_price)}
                            </td>
                            <td
                              title={m.updated_at || ''}
                              style={{ padding: '10px 16px', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}
                            >
                              {fmtTimestamp(m.updated_at)}
                            </td>
                            <td style={{ padding: '10px 16px', textAlign: 'right' }}>
                              <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                                <button onClick={() => startEditPricing(m)} style={{ fontSize: 11, color: 'var(--text-tertiary)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Edit</button>
                                {pricingSrc === 'user' && (
                                  <button onClick={() => resetPricing(m)} style={{ fontSize: 11, color: 'var(--status-red)', padding: '3px 8px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}>Reset</button>
                                )}
                              </div>
                            </td>
                          </>
                        )}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        );
      })()}

      {/* Aliases */}
      <div style={{ marginBottom: 'var(--space-2xl)' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-md)' }}>
          <div>
            <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)' }}>Aliases</h2>
            <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 2 }}>
              Short names for models inside Sage Router. Use the alias as the model name in your tool config.
            </div>
          </div>
          <button
            onClick={() => {
              aliases.value = [...aliases.value, { name: 'new-alias', target: 'provider/model-name' }];
              editingAlias.value = 'new-alias';
              editValue.value = 'provider/model-name';
            }}
            style={{
              padding: '4px 10px', fontSize: 12, color: 'var(--accent)',
              background: 'var(--accent-muted)', borderRadius: 'var(--radius-md)',
              cursor: 'pointer',
            }}
          >
            + Add Alias
          </button>
        </div>
        <div style={{
          background: 'var(--bg-1)', border: '1px solid var(--border)',
          borderRadius: 'var(--radius-lg)', overflow: 'hidden',
        }}>
          <table>
            <thead>
              <tr style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Alias</th>
                <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Target Model</th>
                <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {aliases.value.map(a => <AliasRow key={a.name} alias={a} />)}
            </tbody>
          </table>
        </div>
      </div>

      {/* Combos */}
      <div>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-md)' }}>
          <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)' }}>Combos</h2>
          <button
            onClick={() => {
              showComboModal.value = true;
              comboName.value = '';
              comboModels.value = ['', ''];
            }}
            style={{
              padding: '4px 10px', fontSize: 12, color: 'var(--accent)',
              background: 'var(--accent-muted)', borderRadius: 'var(--radius-md)',
              cursor: 'pointer',
            }}
          >
            + Add Combo
          </button>
        </div>
        {combos.value.length === 0 ? (
          <div style={{ padding: 'var(--space-lg)', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13, background: 'var(--bg-1)', border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)' }}>
            No combos yet. Combos let you define fallback chains across providers.
          </div>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-sm)' }}>
            {combos.value.map(c => <ComboCard key={c.id} combo={c} />)}
          </div>
        )}
      </div>

      {/* Add Combo Modal */}
      {showComboModal.value && (
        <div
          onClick={() => { showComboModal.value = false; }}
          style={{
            position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
            backdropFilter: 'blur(4px)', display: 'flex', alignItems: 'center',
            justifyContent: 'center', zIndex: 9000,
          }}
        >
          <div
            onClick={e => e.stopPropagation()}
            style={{
              background: 'var(--bg-1)', border: '1px solid var(--border-hover)',
              borderRadius: 'var(--radius-xl)', padding: 'var(--space-xl)',
              width: 480, maxWidth: '90vw',
            }}
          >
            <h2 style={{ fontSize: 16, fontWeight: 600, marginBottom: 4 }}>New Combo</h2>
            <p style={{ fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 'var(--space-lg)' }}>
              Define a fallback chain. Requests to this combo name try each model in order.
            </p>

            <div style={{ marginBottom: 'var(--space-md)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>Combo Name</label>
              <input
                type="text"
                placeholder="e.g. fast-fallback"
                value={comboName.value}
                onInput={e => { comboName.value = e.target.value; }}
                style={{
                  width: '100%', padding: '8px 10px', background: 'var(--bg-2)',
                  border: '1px solid var(--border)', borderRadius: 'var(--radius-md)',
                  color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-mono)',
                }}
              />
            </div>

            <div style={{ marginBottom: 'var(--space-lg)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>
                Models (in fallback order)
              </label>
              {comboModels.value.map((model, idx) => (
                <div key={idx} style={{ display: 'flex', alignItems: 'center', gap: 4, marginBottom: 4 }}>
                  <span style={{ fontSize: 11, color: 'var(--text-tertiary)', width: 16, textAlign: 'center', flexShrink: 0 }}>{idx + 1}</span>
                  <select
                    value={model}
                    onChange={e => updateComboEntry(idx, e.target.value)}
                    style={{
                      flex: 1, padding: '6px 8px', background: 'var(--bg-2)',
                      border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
                      color: model ? 'var(--text-primary)' : 'var(--text-tertiary)',
                      fontSize: 12, fontFamily: 'var(--font-mono)',
                    }}
                  >
                    <option value="">Select model...</option>
                    {availableModels.value.map(m => (
                      <option key={m.id} value={m.id}>{m.id}</option>
                    ))}
                  </select>
                  <button
                    onClick={() => moveComboEntry(idx, -1)}
                    disabled={idx === 0}
                    style={{ fontSize: 12, padding: '4px 6px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: idx === 0 ? 'default' : 'pointer', opacity: idx === 0 ? 0.3 : 1, color: 'var(--text-tertiary)' }}
                  >↑</button>
                  <button
                    onClick={() => moveComboEntry(idx, 1)}
                    disabled={idx === comboModels.value.length - 1}
                    style={{ fontSize: 12, padding: '4px 6px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: idx === comboModels.value.length - 1 ? 'default' : 'pointer', opacity: idx === comboModels.value.length - 1 ? 0.3 : 1, color: 'var(--text-tertiary)' }}
                  >↓</button>
                  {comboModels.value.length > 2 && (
                    <button
                      onClick={() => removeComboEntry(idx)}
                      style={{ fontSize: 11, color: 'var(--status-red)', padding: '4px 6px', background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', cursor: 'pointer' }}
                    >×</button>
                  )}
                </div>
              ))}
              <button
                onClick={addComboEntry}
                style={{ fontSize: 11, color: 'var(--accent)', padding: '4px 8px', background: 'none', cursor: 'pointer', marginTop: 4 }}
              >
                + Add fallback
              </button>
            </div>

            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
              <button
                onClick={() => { showComboModal.value = false; }}
                style={{
                  padding: '6px 14px', fontSize: 13, color: 'var(--text-secondary)',
                  background: 'var(--bg-2)', border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)', cursor: 'pointer',
                }}
              >
                Cancel
              </button>
              <button
                onClick={handleSaveCombo}
                style={{
                  padding: '6px 14px', fontSize: 13, color: 'var(--text-primary)',
                  background: 'var(--accent)', borderRadius: 'var(--radius-md)',
                  cursor: 'pointer', fontWeight: 500,
                }}
              >
                Create Combo
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
