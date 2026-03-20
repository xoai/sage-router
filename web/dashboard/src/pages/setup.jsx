import { signal } from '@preact/signals';
import { setupPassword } from '../api/client';

const password = signal('');
const confirm = signal('');
const error = signal('');
const loading = signal(false);

export function SetupPage({ onSuccess }) {
  const handleSubmit = (e) => {
    e.preventDefault();

    if (password.value.length < 8) {
      error.value = 'Password must be at least 8 characters';
      return;
    }
    if (password.value !== confirm.value) {
      error.value = 'Passwords do not match';
      return;
    }

    loading.value = true;
    error.value = '';

    setupPassword(password.value).then(() => {
      password.value = '';
      confirm.value = '';
      onSuccess();
    }).catch(err => {
      error.value = err.message || 'Setup failed';
    }).finally(() => {
      loading.value = false;
    });
  };

  return (
    <div style={{
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      minHeight: '100vh', width: '100%', background: 'var(--bg-0)',
    }}>
      <form onSubmit={handleSubmit} style={{
        width: 380, padding: 'var(--space-xl)',
        background: 'var(--bg-1)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius-xl)',
      }}>
        <div style={{ marginBottom: 'var(--space-xl)', textAlign: 'center' }}>
          <div style={{ fontSize: 18, fontWeight: 600, marginBottom: 4 }}>Welcome to Sage Router</div>
          <div style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>Create a password to secure your dashboard</div>
        </div>

        <div style={{ marginBottom: 'var(--space-md)' }}>
          <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>Password</label>
          <input
            type="password"
            placeholder="Minimum 8 characters"
            value={password.value}
            onInput={e => { password.value = e.target.value; }}
            autoFocus
            style={{
              width: '100%', padding: '10px 12px',
              background: 'var(--bg-2)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)', color: 'var(--text-primary)',
              fontSize: 14, fontFamily: 'var(--font-mono)',
            }}
          />
        </div>

        <div style={{ marginBottom: 'var(--space-md)' }}>
          <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>Confirm Password</label>
          <input
            type="password"
            placeholder="Re-enter password"
            value={confirm.value}
            onInput={e => { confirm.value = e.target.value; }}
            style={{
              width: '100%', padding: '10px 12px',
              background: 'var(--bg-2)', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-md)', color: 'var(--text-primary)',
              fontSize: 14, fontFamily: 'var(--font-mono)',
            }}
          />
        </div>

        {error.value && (
          <div style={{
            marginBottom: 'var(--space-md)', padding: '8px 12px',
            background: 'rgba(255,80,80,0.1)', border: '1px solid rgba(255,80,80,0.2)',
            borderRadius: 'var(--radius-md)', fontSize: 12, color: 'var(--status-red)',
          }}>
            {error.value}
          </div>
        )}

        <button
          type="submit"
          disabled={loading.value}
          style={{
            width: '100%', padding: '10px', fontSize: 14, fontWeight: 500,
            color: 'var(--text-primary)', background: 'var(--accent)',
            borderRadius: 'var(--radius-md)', cursor: 'pointer',
            opacity: loading.value ? 0.6 : 1,
          }}
        >
          {loading.value ? 'Setting up...' : 'Create Password & Continue'}
        </button>

        <div style={{ marginTop: 'var(--space-md)', fontSize: 11, color: 'var(--text-tertiary)', textAlign: 'center' }}>
          You'll use this password to log in to the dashboard.
        </div>
      </form>
    </div>
  );
}
