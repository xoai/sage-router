import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { addToast } from '../components/toast';
import { updateSettings } from '../api/client';

const showPasswordForm = signal(false);
const currentPassword = signal('');
const newPassword = signal('');

const inputStyle = {
  width: '100%',
  padding: '8px 10px',
  background: 'var(--bg-2)',
  border: '1px solid var(--border)',
  borderRadius: 'var(--radius-md)',
  color: 'var(--text-primary)',
  fontSize: 13,
};

const btnStyle = {
  padding: '6px 14px',
  fontSize: 13,
  color: 'var(--text-primary)',
  background: 'var(--accent)',
  borderRadius: 'var(--radius-md)',
  cursor: 'pointer',
  fontWeight: 500,
};

const btnSecondary = {
  padding: '6px 14px',
  fontSize: 13,
  color: 'var(--text-secondary)',
  background: 'var(--bg-2)',
  border: '1px solid var(--border)',
  borderRadius: 'var(--radius-md)',
  cursor: 'pointer',
};

function handleExport() {
  addToast('Export not yet implemented', 'info');
}

function handleImport() {
  addToast('Import not yet implemented', 'info');
}

export function SettingsPage() {
  return (
    <div style={{ padding: 'var(--space-2xl)', maxWidth: 720, width: '100%' }}>
      <h1 style={{ fontSize: 20, fontWeight: 600, marginBottom: 'var(--space-xl)' }}>Settings</h1>

      {/* Password */}
      <section style={{ marginBottom: 'var(--space-2xl)' }}>
        <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)', marginBottom: 'var(--space-md)' }}>Password</h2>

        {!showPasswordForm.value ? (
          <button onClick={() => { showPasswordForm.value = true; }} style={btnSecondary}>
            Change Password
          </button>
        ) : (
          <div style={{
            background: 'var(--bg-1)', border: '1px solid var(--border)',
            borderRadius: 'var(--radius-lg)', padding: 'var(--space-lg)',
          }}>
            <div style={{ marginBottom: 'var(--space-md)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>Current Password</label>
              <input
                type="password"
                value={currentPassword.value}
                onInput={e => { currentPassword.value = e.target.value; }}
                style={inputStyle}
              />
            </div>
            <div style={{ marginBottom: 'var(--space-lg)' }}>
              <label style={{ display: 'block', fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 4 }}>New Password</label>
              <input
                type="password"
                value={newPassword.value}
                onInput={e => { newPassword.value = e.target.value; }}
                style={inputStyle}
              />
            </div>
            <div style={{ display: 'flex', gap: 8 }}>
              <button
                onClick={() => {
                  updateSettings({ password: newPassword.value, current_password: currentPassword.value }).then(() => {
                    addToast('Password updated', 'success');
                    showPasswordForm.value = false;
                    currentPassword.value = '';
                    newPassword.value = '';
                  }).catch(err => {
                    addToast('Failed: ' + err.message, 'error');
                  });
                }}
                style={btnStyle}
              >
                Update Password
              </button>
              <button
                onClick={() => { showPasswordForm.value = false; }}
                style={btnSecondary}
              >
                Cancel
              </button>
            </div>
          </div>
        )}
      </section>

      {/* Import/Export */}
      <section>
        <h2 style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-secondary)', marginBottom: 'var(--space-md)' }}>Data</h2>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={handleExport} style={btnSecondary}>
            Export Settings
          </button>
          <button onClick={handleImport} style={btnSecondary}>
            Import Settings
          </button>
        </div>
      </section>
    </div>
  );
}
