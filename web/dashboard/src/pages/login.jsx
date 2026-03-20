import { signal } from '@preact/signals';
import { login } from '../api/client';

const password = signal('');
const error = signal('');
const loading = signal(false);

export function LoginPage({ onSuccess }) {
  const handleSubmit = (e) => {
    e.preventDefault();
    if (!password.value.trim()) return;

    loading.value = true;
    error.value = '';

    login(password.value).then(() => {
      password.value = '';
      onSuccess();
    }).catch(err => {
      error.value = err.status === 401 ? 'Invalid password' : 'Login failed';
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
        width: 340, padding: 'var(--space-xl)',
        background: 'var(--bg-1)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius-xl)',
      }}>
        <div style={{ marginBottom: 'var(--space-xl)', textAlign: 'center' }}>
          <div style={{ fontSize: 18, fontWeight: 600, marginBottom: 4 }}>Sage Router</div>
          <div style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>Enter your password to continue</div>
        </div>

        <div style={{ marginBottom: 'var(--space-md)' }}>
          <input
            type="password"
            placeholder="Password"
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
          {loading.value ? 'Signing in...' : 'Sign In'}
        </button>
      </form>
    </div>
  );
}
