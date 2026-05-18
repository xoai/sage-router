import { useSignal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { updateKey } from '../api/client';
import { addToast } from './toast';
import { ModelPicker } from './model-picker';
import { commit, parseEntries } from './model-picker-helpers.js';

// KeyEditModal — single-screen edit modal for an existing API key.
// Identity (Name, Prefix) is IMMUTABLE post-create. Only quotas + policy
// are editable. Save uses PATCH /api/keys/{id} (plan-review M3 — the
// route is PATCH, not PUT; verified at server.go:164 + client.js:138).
//
// AC-E1..E3 of 20260516-keys-management-redesign.

const ROUTING_OPTIONS = [
  { value: '', label: 'Default (system-wide setting)' },
  { value: 'fast', label: 'fast (lowest latency)' },
  { value: 'balanced', label: 'balanced (default mix)' },
  { value: 'cheap', label: 'cheap (lowest cost)' },
  { value: 'best', label: 'best (highest quality)' },
  { value: 'user-order', label: 'User order (try in the order you set)' },
];

export function KeyEditModal({ keyRow, onClose, onSaved }) {
  // Initialize form from the row — useSignal evaluates the initial
  // expression ONCE per mount, so editing one field (which triggers
  // a re-render) no longer resets the others. Parent (keys.jsx)
  // unmounts/remounts the modal between rows via conditional render
  // on editingKey.value, so per-mount init is correct.
  const budgetMonthly = useSignal(String(keyRow.budget_monthly ?? 0));
  const budgetHardLimit = useSignal(keyRow.budget_hard_limit ?? false);
  const rateLimitRPM = useSignal(String(keyRow.rate_limit_rpm ?? 0));
  const allowedModels = useSignal(keyRow.allowed_models || '*');
  const routingStrategy = useSignal(keyRow.routing_strategy || '');
  const saving = useSignal(false);
  const dirtyConfirm = useSignal(false);

  // ESC handler — close modal.
  // Cycle 20260516-edit-modal-close-confirm: dismiss the dirty-confirm
  // overlay before falling through to handleClose, matching wizard's
  // pattern at key-create-wizard.jsx:67-83. `[]` deps + signal `.value`
  // reads at call time → handler closure binds to the stable signal
  // refs, no re-registration needed.
  useEffect(() => {
    const onKey = (e) => {
      if (e.key !== 'Escape') return;
      if (dirtyConfirm.value) {
        dirtyConfirm.value = false;
        return;
      }
      handleClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  // isDirty compares CURRENT field values against keyRow BASELINE.
  // Cycle 20260516-edit-modal-close-confirm — per self-learning
  // [41f8388f…] point 6, edit modals must compare against the keyRow
  // prop, NOT against static defaults (which is what the wizard does).
  //
  // CRITICAL: comparisons must mirror handleSave's persistence behavior,
  // not just the useSignal initializer expressions:
  //   - Numeric fields (budgetMonthly, rateLimitRPM) compared via
  //     `Number(x) || 0` to match handleSave's coercion at lines 56/58.
  //     Without this, clearing a `0`-baseline numeric field to `''`
  //     falsely flags dirty.
  //   - allowedModels canonicalized via commit(parseEntries(...)) for
  //     dedup + whitespace-trim + floor-invariant normalization. Note:
  //     cycle 20260516-routing-strategy-ux M4 DROPPED the alphabetical
  //     sort in commit() — order changes (drag-reorder, keyboard ↑/↓
  //     reorder) ARE meaningful edits now (per AC-H6), so isDirty
  //     correctly fires on reorder-only changes via this comparison.
  //     canonicalize is now: dedup + trim + floor-invariant — no sort.
  function canonicalizeModels(s) {
    return commit('', parseEntries(s));
  }
  function isDirty() {
    return (Number(budgetMonthly.value) || 0) !== (Number(keyRow.budget_monthly) || 0)
      || budgetHardLimit.value !== (keyRow.budget_hard_limit ?? false)
      || (Number(rateLimitRPM.value) || 0) !== (Number(keyRow.rate_limit_rpm) || 0)
      || canonicalizeModels(allowedModels.value) !== canonicalizeModels(keyRow.allowed_models || '*')
      || routingStrategy.value !== (keyRow.routing_strategy || '');
  }

  // 3-branch close state machine (per cycle 20260516-edit-modal-close-confirm,
  // mirrors wizard's 4-branch pattern with reveal stage collapsed away):
  //   1. saving.value → BLOCK (no AbortController; mid-flight PATCH must complete)
  //   2. isDirty() → show discard-confirm overlay
  //   3. else → clean onClose()
  function handleClose() {
    if (saving.value) return;
    if (isDirty()) {
      dirtyConfirm.value = true;
      return;
    }
    onClose();
  }

  // Intentional bypass — called from the discard-confirm dialog's
  // [Discard] button. Does NOT call handleClose (would recurse into
  // confirm since fields are still dirty).
  function handleDiscardAnyway() {
    dirtyConfirm.value = false;
    onClose();
  }

  function handleSave() {
    saving.value = true;
    updateKey(keyRow.id, {
      budget_monthly: Number(budgetMonthly.value) || 0,
      budget_hard_limit: budgetHardLimit.value,
      rate_limit_rpm: Number(rateLimitRPM.value) || 0,
      allowed_models: allowedModels.value || '*',
      routing_strategy: routingStrategy.value,
    }).then(() => {
      saving.value = false;
      addToast('Key updated', 'success');
      if (onSaved) onSaved();
      onClose();
    }).catch(err => {
      saving.value = false;
      addToast('Update failed: ' + (err?.message || 'unknown error'), 'error');
    });
  }

  const inputStyle = {
    width: '100%',
    padding: '8px 10px',
    background: 'var(--bg-2)',
    border: '1px solid var(--border)',
    borderRadius: 'var(--radius-md)',
    color: 'var(--text-primary)',
    fontSize: 13,
  };
  const labelStyle = {
    display: 'block',
    fontSize: 12,
    color: 'var(--text-tertiary)',
    marginBottom: 4,
  };

  return (
    <>
    <div
      onClick={handleClose}
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
          width: 520, maxWidth: '92vw', maxHeight: '90vh', overflow: 'auto',
        }}
      >
        <h2 style={{ fontSize: 16, fontWeight: 600, marginBottom: 'var(--space-md)' }}>
          Edit API Key
        </h2>

        {/* Read-only identity */}
        <div style={{
          padding: 'var(--space-sm)',
          background: 'var(--bg-2)',
          borderRadius: 'var(--radius-md)',
          marginBottom: 'var(--space-lg)',
          fontSize: 12,
        }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 4 }}>
            <span style={{ color: 'var(--text-tertiary)' }}>Name</span>
            <span style={{ color: 'var(--text-primary)' }}>{keyRow.name}</span>
          </div>
          <div style={{ display: 'flex', justifyContent: 'space-between' }}>
            <span style={{ color: 'var(--text-tertiary)' }}>Prefix</span>
            <span style={{ color: 'var(--text-primary)', fontFamily: 'var(--font-mono)' }}>
              {keyRow.prefix}…
            </span>
          </div>
          <div style={{ fontSize: 10, color: 'var(--text-tertiary)', marginTop: 6 }}>
            Name and prefix can't be changed. To rotate the secret, delete this key and create a new one.
          </div>
        </div>

        {/* Editable: quotas */}
        <div style={{ marginBottom: 'var(--space-md)' }}>
          <label style={labelStyle}>Monthly budget (USD)</label>
          <input
            type="number"
            step="0.01"
            min="0"
            placeholder="0 = unlimited"
            value={budgetMonthly.value}
            onInput={e => { budgetMonthly.value = e.target.value; }}
            style={inputStyle}
          />
        </div>

        <label style={{
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          fontSize: 13,
          color: 'var(--text-primary)',
          cursor: 'pointer',
          marginBottom: 'var(--space-md)',
        }}>
          <input
            type="checkbox"
            checked={budgetHardLimit.value}
            onChange={e => { budgetHardLimit.value = e.target.checked; }}
          />
          <span>Hard limit — block requests after cap is reached</span>
        </label>

        <div style={{ marginBottom: 'var(--space-md)' }}>
          <label style={labelStyle}>Rate limit (requests per minute)</label>
          <input
            type="number"
            step="1"
            min="0"
            placeholder="0 = unlimited"
            value={rateLimitRPM.value}
            onInput={e => { rateLimitRPM.value = e.target.value; }}
            style={inputStyle}
          />
        </div>

        {/* Editable: policy */}
        <div style={{ marginBottom: 'var(--space-md)' }}>
          <label style={labelStyle}>Allowed models</label>
          <ModelPicker
            value={allowedModels.value}
            onChange={v => { allowedModels.value = v; }}
            disabled={saving.value}
          />
        </div>

        <div style={{ marginBottom: 'var(--space-lg)' }}>
          <label style={labelStyle}>Routing strategy override</label>
          <select
            value={routingStrategy.value}
            onChange={e => { routingStrategy.value = e.target.value; }}
            style={inputStyle}
          >
            {ROUTING_OPTIONS.map(o => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
        </div>

        {/* Footer */}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button
            onClick={handleClose}
            style={{
              padding: '6px 14px', fontSize: 13,
              color: 'var(--text-secondary)',
              background: 'var(--bg-2)',
              border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)',
              cursor: 'pointer',
            }}
          >
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={saving.value}
            style={{
              padding: '6px 14px', fontSize: 13,
              color: 'var(--text-primary)',
              background: 'var(--accent)',
              border: 'none',
              borderRadius: 'var(--radius-md)',
              cursor: saving.value ? 'not-allowed' : 'pointer',
              fontWeight: 500,
              opacity: saving.value ? 0.6 : 1,
            }}
          >
            {saving.value ? 'Saving...' : 'Save'}
          </button>
        </div>
      </div>
    </div>

    {/* Discard-changes confirm overlay (cycle 20260516-edit-modal-close-confirm).
        Mirrors wizard's dirtyConfirm overlay at key-create-wizard.jsx:594-647
        exactly (zIndex 9100 above modal backdrop 9000; outer onClick dismisses;
        inner stopPropagation; primary [Keep editing] / secondary [Discard]). */}
    {dirtyConfirm.value && (
      <div
        onClick={() => { dirtyConfirm.value = false; }}
        style={{
          position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          zIndex: 9100,
        }}
      >
        <div
          onClick={e => e.stopPropagation()}
          style={{
            background: 'var(--bg-1)',
            border: '1px solid var(--border-hover)',
            borderRadius: 'var(--radius-xl)',
            padding: 'var(--space-xl)',
            width: 420,
            maxWidth: '92vw',
          }}
        >
          <h3 style={{ fontSize: 14, fontWeight: 600, marginBottom: 'var(--space-md)' }}>
            Discard your changes?
          </h3>
          <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 'var(--space-lg)', lineHeight: 1.4 }}>
            You've edited this key's settings. Closing now will discard the changes — the key itself stays as-is.
          </div>
          <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
            <button
              onClick={() => { dirtyConfirm.value = false; }}
              style={{
                padding: '6px 14px', fontSize: 13,
                color: 'var(--text-primary)',
                background: 'var(--accent)',
                border: 'none',
                borderRadius: 'var(--radius-md)',
                cursor: 'pointer',
                fontWeight: 500,
              }}
            >
              Keep editing
            </button>
            <button
              onClick={handleDiscardAnyway}
              style={{
                padding: '6px 14px', fontSize: 13,
                color: 'var(--text-secondary)',
                background: 'var(--bg-2)',
                border: '1px solid var(--border)',
                borderRadius: 'var(--radius-md)',
                cursor: 'pointer',
              }}
            >
              Discard
            </button>
          </div>
        </div>
      </div>
    )}
    </>
  );
}
