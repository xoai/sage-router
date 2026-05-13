import { acceptTOS } from '../api/oauth';
import { addToast } from './toast';

// TosModal — shown on first subscription action when the backend returns
// 412 {requires_tos:true, message}. Acceptance is idempotent server-side
// (AC40) and persists across restarts, so this only appears once per
// sage-router instance.
//
// onAccept: invoked after a successful POST /api/auth/tos/accept; the
// caller should retry the original action.
export function TosModal({ message, onAccept, onCancel }) {
  return (
    <div
      onClick={onCancel}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
        backdropFilter: 'blur(4px)', display: 'flex', alignItems: 'center',
        justifyContent: 'center', zIndex: 9500,
      }}
    >
      <div
        onClick={e => e.stopPropagation()}
        style={{
          background: 'var(--bg-1)', border: '1px solid var(--border-hover)',
          borderRadius: 'var(--radius-xl)', padding: 'var(--space-xl)',
          width: 520, maxWidth: '92vw', maxHeight: '80vh',
          display: 'flex', flexDirection: 'column',
        }}
      >
        <h2 style={{ fontSize: 16, fontWeight: 600, marginBottom: 'var(--space-md)' }}>
          Before you connect a subscription
        </h2>
        <div style={{
          fontSize: 12, color: 'var(--text-secondary)', lineHeight: 1.6,
          background: 'var(--bg-2)', padding: 'var(--space-md)',
          borderRadius: 'var(--radius-md)', overflow: 'auto', maxHeight: 360,
          whiteSpace: 'pre-wrap', fontFamily: 'var(--font-mono)',
          marginBottom: 'var(--space-md)',
        }}>
          {message}
        </div>
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button
            onClick={onCancel}
            style={{
              padding: '6px 14px', fontSize: 13, color: 'var(--text-secondary)',
              background: 'var(--bg-2)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)', cursor: 'pointer',
            }}
          >
            Cancel
          </button>
          <button
            onClick={() => {
              acceptTOS().then(res => {
                if (!res.ok) {
                  addToast('Could not record acceptance: ' + (res.data?.error || res.status), 'error');
                  return;
                }
                onAccept();
              }).catch(err => {
                addToast('Could not record acceptance: ' + err.message, 'error');
              });
            }}
            style={{
              padding: '6px 14px', fontSize: 13, color: 'var(--text-primary)',
              background: 'var(--accent)', borderRadius: 'var(--radius-md)',
              cursor: 'pointer', fontWeight: 500,
            }}
          >
            I understand &amp; accept
          </button>
        </div>
      </div>
    </div>
  );
}
