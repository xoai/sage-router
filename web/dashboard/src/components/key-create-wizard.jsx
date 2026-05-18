import { useSignal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { createKey } from '../api/client';
import { addToast } from './toast';
import { ModelPicker } from './model-picker';

// KeyCreateWizard — 4-step modal wizard for creating an API key.
//
// Steps:
//   1. Identity:  Name (required)
//   2. Quotas:    BudgetMonthly, BudgetHardLimit, RateLimitRPM
//   3. Policy:    AllowedModels pattern, RoutingStrategy
//   4. Review:    summary + [Create Key]
//   then Reveal:  one-time-display of the plaintext key
//
// AC-D1..D7 of 20260516-keys-management-redesign.
//
// Spec-review m2 — validation gating:
//   Step 1 forward-block on empty name; Steps 2-3 non-blocking inline
//   hints; Step 4 Create strictly validates all prior fields.
//
// Spec-review m3 — copy-detection mechanism:
//   `copied` signal flips on [Copy] button click (not clipboard event).
//   Close-without-copy at reveal step triggers confirm dialog.

const ROUTING_OPTIONS = [
  { value: '', label: 'Default (system-wide setting)' },
  { value: 'fast', label: 'fast (lowest latency)' },
  { value: 'balanced', label: 'balanced (default mix)' },
  { value: 'cheap', label: 'cheap (lowest cost)' },
  { value: 'best', label: 'best (highest quality)' },
  { value: 'user-order', label: 'User order (try in the order you set)' },
];

export function KeyCreateWizard({ onClose, onCreated }) {
  // Step state — useSignal: per-mount stable (fresh per wizard open,
  // survives re-renders). signal() inside body would allocate a new
  // Signal each render and lose all local state.
  const step = useSignal(1);
  const creating = useSignal(false);
  const revealedKey = useSignal(null);
  const copied = useSignal(false);
  const closingConfirm = useSignal(false);
  const dirtyConfirm = useSignal(false);

  // Form fields
  const name = useSignal('');
  const budgetMonthly = useSignal('');
  const budgetHardLimit = useSignal(false);
  const rateLimitRPM = useSignal('');
  const allowedModels = useSignal('*');
  const routingStrategy = useSignal('');

  // Validation
  const nameError = useSignal('');
  const formErrors = useSignal({}); // {fieldName: message}

  // Total steps: 4 wizard steps + 1 reveal screen
  const TOTAL_STEPS = 4;
  const atReveal = () => revealedKey.value != null;

  // ESC handler — global keydown listener.
  // Reveal screen: ESC triggers confirm-without-copy if !copied.
  // Pre-reveal with dirty form: ESC triggers discard-confirm.
  // When either confirm dialog is itself visible, ESC dismisses the confirm
  // (NOT a recursive close-attempt — fixes post-review m4 stacking glitch).
  // Order between closingConfirm and dirtyConfirm dismisses is arbitrary —
  // they are mutually exclusive by construction (atReveal() partitions them).
  useEffect(() => {
    const onKey = (e) => {
      if (e.key !== 'Escape') return;
      if (closingConfirm.value) {
        closingConfirm.value = false;
        return;
      }
      if (dirtyConfirm.value) {
        dirtyConfirm.value = false;
        return;
      }
      handleClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  // Treats "user reverted their own edit back to initial" as not-dirty —
  // closing recovers no information they didn't already throw away.
  function isDirty() {
    return name.value.trim() !== ''
      || budgetMonthly.value !== ''
      || budgetHardLimit.value
      || rateLimitRPM.value !== ''
      || allowedModels.value !== '*'
      || routingStrategy.value !== '';
  }

  function handleClose() {
    // Block close while the create request is in flight — matches the
    // disabled state of the Create button (handleCreate has no AbortController,
    // so closing mid-flight would still create the key server-side).
    if (creating.value) return;
    if (atReveal() && !copied.value) {
      closingConfirm.value = true;
      return;
    }
    if (!atReveal() && isDirty()) {
      dirtyConfirm.value = true;
      return;
    }
    onClose();
  }

  function validateAll() {
    const errs = {};
    if (!name.value.trim()) errs.name = 'Name is required';
    if (budgetMonthly.value !== '' && Number(budgetMonthly.value) < 0) {
      errs.budgetMonthly = 'Must be ≥ 0';
    }
    if (rateLimitRPM.value !== '') {
      const n = Number(rateLimitRPM.value);
      if (!Number.isInteger(n) || n < 0) errs.rateLimitRPM = 'Must be a non-negative integer';
    }
    formErrors.value = errs;
    return Object.keys(errs).length === 0;
  }

  function gotoNext() {
    if (step.value === 1) {
      if (!name.value.trim()) {
        nameError.value = 'Name is required';
        return;
      }
      nameError.value = '';
    }
    step.value = Math.min(step.value + 1, TOTAL_STEPS);
  }

  function gotoBack() {
    step.value = Math.max(step.value - 1, 1);
  }

  function handleCreate() {
    if (!validateAll()) {
      // Jump back to first step with an error
      if (formErrors.value.name) step.value = 1;
      else if (formErrors.value.budgetMonthly || formErrors.value.rateLimitRPM) step.value = 2;
      return;
    }
    creating.value = true;
    createKey({
      name: name.value.trim(),
      budget_monthly: budgetMonthly.value === '' ? 0 : Number(budgetMonthly.value),
      budget_hard_limit: budgetHardLimit.value,
      allowed_models: allowedModels.value || '*',
      rate_limit_rpm: rateLimitRPM.value === '' ? 0 : Number(rateLimitRPM.value),
      routing_strategy: routingStrategy.value,
    }).then(res => {
      creating.value = false;
      if (!res?.key) {
        addToast('Create failed: no key returned', 'error');
        return;
      }
      revealedKey.value = res.key;
    }).catch(err => {
      creating.value = false;
      addToast('Create failed: ' + (err?.message || 'unknown error'), 'error');
    });
  }

  function handleCopy() {
    if (!revealedKey.value) return;
    navigator.clipboard?.writeText(revealedKey.value).catch(() => {});
    copied.value = true;
    addToast('Key copied to clipboard', 'success');
  }

  function handleDone() {
    if (onCreated) onCreated();
    onClose();
  }

  function handleCloseAnyway() {
    closingConfirm.value = false;
    onClose();
  }

  function handleDiscardAnyway() {
    dirtyConfirm.value = false;
    onClose();
  }

  // Inputs
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
          {/* Header */}
          <div style={{ marginBottom: 'var(--space-lg)' }}>
            <h2 style={{ fontSize: 16, fontWeight: 600 }}>
              {atReveal() ? 'API Key Created' : 'Create API Key'}
            </h2>
            {!atReveal() && (
              <div style={{
                fontSize: 11,
                color: 'var(--text-tertiary)',
                marginTop: 4,
                fontFamily: 'var(--font-mono)',
              }}>
                Step {step.value} of {TOTAL_STEPS}
              </div>
            )}
          </div>

          {/* Reveal screen */}
          {atReveal() && (
            <>
              <div style={{
                padding: 'var(--space-md)',
                background: 'var(--accent-muted)',
                border: '1px solid var(--accent)',
                borderRadius: 'var(--radius-md)',
                marginBottom: 'var(--space-md)',
                fontSize: 12,
                color: 'var(--text-secondary)',
                lineHeight: 1.4,
              }}>
                <strong style={{ color: 'var(--text-primary)' }}>Copy this key now.</strong> It
                won't be shown again — sage-router stores only the hash.
              </div>
              <div style={{
                padding: 'var(--space-md)',
                background: 'var(--bg-2)',
                border: '1px solid var(--border)',
                borderRadius: 'var(--radius-md)',
                fontFamily: 'var(--font-mono)',
                fontSize: 13,
                wordBreak: 'break-all',
                marginBottom: 'var(--space-md)',
                color: 'var(--text-primary)',
              }}>
                {revealedKey.value}
              </div>
              <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
                <button
                  onClick={handleCopy}
                  style={{
                    padding: '6px 14px', fontSize: 13,
                    background: copied.value ? 'var(--bg-2)' : 'var(--accent)',
                    color: 'var(--text-primary)',
                    border: copied.value ? '1px solid var(--border)' : 'none',
                    borderRadius: 'var(--radius-md)', cursor: 'pointer', fontWeight: 500,
                  }}
                >
                  {copied.value ? '✓ Copied' : 'Copy'}
                </button>
                <button
                  onClick={handleDone}
                  disabled={!copied.value}
                  style={{
                    padding: '6px 14px', fontSize: 13,
                    background: copied.value ? 'var(--accent)' : 'var(--bg-2)',
                    color: 'var(--text-primary)',
                    border: copied.value ? 'none' : '1px solid var(--border)',
                    borderRadius: 'var(--radius-md)',
                    cursor: copied.value ? 'pointer' : 'not-allowed',
                    fontWeight: 500,
                    opacity: copied.value ? 1 : 0.5,
                  }}
                >
                  Done
                </button>
              </div>
            </>
          )}

          {/* Step 1: Identity */}
          {!atReveal() && step.value === 1 && (
            <>
              <label style={labelStyle}>Name <span style={{ color: 'var(--status-red)' }}>*</span></label>
              <input
                type="text"
                placeholder="e.g. prod-team"
                value={name.value}
                onInput={e => { name.value = e.target.value; if (nameError.value) nameError.value = ''; }}
                style={{
                  ...inputStyle,
                  borderColor: nameError.value ? 'var(--status-red)' : 'var(--border)',
                }}
                autoFocus
              />
              {nameError.value && (
                <div style={{ fontSize: 11, color: 'var(--status-red)', marginTop: 4 }}>
                  {nameError.value}
                </div>
              )}
              <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 6 }}>
                A descriptive label for this key. Visible to admins on the API Keys page.
              </div>
            </>
          )}

          {/* Step 2: Quotas */}
          {!atReveal() && step.value === 2 && (
            <>
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
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 4 }}>
                  Soft cap by default; flip the toggle below to enforce.
                </div>
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

              <div>
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
            </>
          )}

          {/* Step 3: Policy */}
          {!atReveal() && step.value === 3 && (
            <>
              <div style={{ marginBottom: 'var(--space-md)' }}>
                <label style={labelStyle}>Allowed models</label>
                <ModelPicker
                  value={allowedModels.value}
                  onChange={v => { allowedModels.value = v; }}
                  disabled={creating.value}
                />
              </div>

              <div>
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
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 4 }}>
                  Overrides the system-wide default for requests using <code>auto</code>.
                </div>
              </div>
            </>
          )}

          {/* Step 4: Review */}
          {!atReveal() && step.value === 4 && (
            <div style={{ fontSize: 13 }}>
              <ReviewRow label="Name" value={name.value || <em style={{ color: 'var(--status-red)' }}>required</em>} />
              <ReviewRow
                label="Monthly budget"
                value={budgetMonthly.value !== '' && Number(budgetMonthly.value) > 0
                  ? `$${Number(budgetMonthly.value).toFixed(2)}${budgetHardLimit.value ? ' (hard limit)' : ' (soft)'}`
                  : 'unlimited'}
              />
              <ReviewRow
                label="Rate limit"
                value={rateLimitRPM.value !== '' && Number(rateLimitRPM.value) > 0
                  ? `${rateLimitRPM.value} RPM`
                  : 'unlimited'}
              />
              <ReviewRow label="Allowed models" value={allowedModels.value || '*'} />
              <ReviewRow
                label="Routing strategy"
                value={routingStrategy.value || 'default'}
              />
              {Object.keys(formErrors.value).length > 0 && (
                <div style={{
                  marginTop: 12,
                  padding: 'var(--space-sm)',
                  background: 'rgba(239, 68, 68, 0.1)',
                  border: '1px solid var(--status-red)',
                  borderRadius: 'var(--radius-sm)',
                  fontSize: 11,
                  color: 'var(--status-red)',
                }}>
                  Fix the following before creating:
                  <ul style={{ margin: '4px 0 0 16px' }}>
                    {Object.entries(formErrors.value).map(([k, v]) => <li key={k}>{v}</li>)}
                  </ul>
                </div>
              )}
            </div>
          )}

          {/* Wizard footer */}
          {!atReveal() && (
            <div style={{
              display: 'flex',
              gap: 8,
              justifyContent: 'flex-end',
              marginTop: 'var(--space-lg)',
            }}>
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
              {step.value > 1 && (
                <button
                  onClick={gotoBack}
                  style={{
                    padding: '6px 14px', fontSize: 13,
                    color: 'var(--text-secondary)',
                    background: 'var(--bg-2)',
                    border: '1px solid var(--border)',
                    borderRadius: 'var(--radius-md)',
                    cursor: 'pointer',
                  }}
                >
                  ← Back
                </button>
              )}
              {step.value < TOTAL_STEPS ? (
                <button
                  onClick={gotoNext}
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
                  Next →
                </button>
              ) : (
                <button
                  onClick={handleCreate}
                  disabled={creating.value}
                  style={{
                    padding: '6px 14px', fontSize: 13,
                    color: 'var(--text-primary)',
                    background: 'var(--accent)',
                    border: 'none',
                    borderRadius: 'var(--radius-md)',
                    cursor: creating.value ? 'not-allowed' : 'pointer',
                    fontWeight: 500,
                    opacity: creating.value ? 0.6 : 1,
                  }}
                >
                  {creating.value ? 'Creating...' : 'Create Key'}
                </button>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Close-without-copy confirm overlay */}
      {closingConfirm.value && (
        <div
          onClick={() => { closingConfirm.value = false; }}
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
              Close without copying?
            </h3>
            <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 'var(--space-lg)', lineHeight: 1.4 }}>
              You haven't copied the key yet. It won't be shown again — sage-router only stores the hash. Delete this key and create a new one if you need to recover it.
            </div>
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
              <button
                onClick={() => { closingConfirm.value = false; }}
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
                Cancel
              </button>
              <button
                onClick={handleCloseAnyway}
                style={{
                  padding: '6px 14px', fontSize: 13,
                  color: 'var(--text-secondary)',
                  background: 'var(--bg-2)',
                  border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-md)',
                  cursor: 'pointer',
                }}
              >
                Close anyway
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Discard-changes confirm overlay (pre-reveal dirty form).
          NOTE: near-duplicate of the close-without-copy overlay above —
          intentional duplication for minimal-change. Lift to a
          <ConfirmOverlay> helper if a third confirm gets added. */}
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
              You've entered information for this key. Closing now will discard it — you'd need to start over.
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

function ReviewRow({ label, value }) {
  return (
    <div style={{
      display: 'flex',
      justifyContent: 'space-between',
      padding: '6px 0',
      borderBottom: '1px solid var(--border)',
    }}>
      <span style={{ color: 'var(--text-tertiary)' }}>{label}</span>
      <span style={{ color: 'var(--text-primary)', fontFamily: 'var(--font-mono)' }}>{value}</span>
    </div>
  );
}
