import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { addToast } from '../components/toast';
import { getAliases, setAlias, deleteAlias, getCombos, createCombo, deleteCombo, getModels } from '../api/client';

const aliases = signal([]);
const combos = signal([]);
const availableModels = signal([]);
const providerFilter = signal('all');

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
  }, []);

  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 960, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Models</h1>

      {/* Available Models */}
      {availableModels.value.length > 0 && (() => {
        const providers = [...new Set(availableModels.value.map(m => m.provider))];
        const filtered = providerFilter.value === 'all'
          ? availableModels.value
          : availableModels.value.filter(m => m.provider === providerFilter.value);
        return (
          <div style={{ marginBottom: 'var(--space-2xl)' }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-md)' }}>
              <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)' }}>
                Available Models
                <span style={{ fontWeight: 400, color: 'var(--text-tertiary)', marginLeft: 6, fontSize: 12 }}>
                  ({filtered.length})
                </span>
              </h2>
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
              borderRadius: 'var(--radius-lg)', overflow: 'hidden',
            }}>
              <table>
                <thead>
                  <tr style={{ fontSize: 11, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                    <th style={{ padding: '8px 16px', textAlign: 'left', fontWeight: 500 }}>Model</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Input $/M</th>
                    <th style={{ padding: '8px 16px', textAlign: 'right', fontWeight: 500 }}>Output $/M</th>
                  </tr>
                </thead>
                <tbody>
                  {filtered.map(m => (
                    <tr key={m.id} style={{ borderTop: '1px solid var(--border)' }}>
                      <td style={{ padding: '10px 16px' }}>
                        <span style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{m.id}</span>
                        {m.display_name && <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 8 }}>{m.display_name}</span>}
                      </td>
                      <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}>
                        {m.input_price ? '$' + m.input_price.toFixed(2) : '-'}
                      </td>
                      <td style={{ padding: '10px 16px', fontFamily: 'var(--font-mono)', fontSize: 11, textAlign: 'right', color: 'var(--text-tertiary)' }}>
                        {m.output_price ? '$' + m.output_price.toFixed(2) : '-'}
                      </td>
                    </tr>
                  ))}
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
