import { signal } from '@preact/signals';

const toasts = signal([]);
let nextId = 0;

export function addToast(message, type = 'info', duration = 3000) {
  const id = ++nextId;
  toasts.value = [...toasts.value, { id, message, type }];
  setTimeout(() => {
    toasts.value = toasts.value.filter(t => t.id !== id);
  }, duration);
}

const typeStyles = {
  info: { bg: 'var(--bg-3)', border: 'var(--accent)' },
  success: { bg: 'var(--bg-3)', border: 'var(--status-green)' },
  error: { bg: 'var(--bg-3)', border: 'var(--status-red)' },
  warning: { bg: 'var(--bg-3)', border: 'var(--status-yellow)' },
};

export function ToastContainer() {
  if (toasts.value.length === 0) return null;

  return (
    <div style={{
      position: 'fixed',
      bottom: 20,
      right: 20,
      display: 'flex',
      flexDirection: 'column',
      gap: 8,
      zIndex: 10000,
      pointerEvents: 'none',
    }}>
      {toasts.value.map(t => {
        const ts = typeStyles[t.type] || typeStyles.info;
        return (
          <div
            key={t.id}
            style={{
              padding: '10px 16px',
              background: ts.bg,
              borderLeft: `3px solid ${ts.border}`,
              borderRadius: 'var(--radius-md)',
              color: 'var(--text-primary)',
              fontSize: 13,
              fontFamily: 'var(--font-sans)',
              boxShadow: '0 4px 24px rgba(0,0,0,0.4)',
              pointerEvents: 'auto',
              animation: 'slideIn 200ms ease',
            }}
          >
            {t.message}
          </div>
        );
      })}
    </div>
  );
}
