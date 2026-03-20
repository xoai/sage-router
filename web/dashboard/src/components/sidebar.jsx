import { useLocation } from 'wouter-preact';
import { logout } from '../api/client';
import { authState } from '../app';

const navItems = [
  {
    path: '/',
    label: 'Overview',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <rect x="3" y="3" width="7" height="7" rx="1" />
        <rect x="14" y="3" width="7" height="7" rx="1" />
        <rect x="3" y="14" width="7" height="7" rx="1" />
        <rect x="14" y="14" width="7" height="7" rx="1" />
      </svg>
    ),
  },
  {
    path: '/providers',
    label: 'Providers',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <path d="M22 12h-4l-3 9L9 3l-3 9H2" />
      </svg>
    ),
  },
  {
    path: '/models',
    label: 'Models',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <path d="M12 2L2 7l10 5 10-5-10-5z" />
        <path d="M2 17l10 5 10-5" />
        <path d="M2 12l10 5 10-5" />
      </svg>
    ),
  },
  {
    path: '/usage',
    label: 'Usage',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <path d="M18 20V10" /><path d="M12 20V4" /><path d="M6 20v-6" />
      </svg>
    ),
  },
  {
    path: '/routing',
    label: 'Routing',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <circle cx="12" cy="12" r="2" />
        <path d="M16.24 7.76a6 6 0 010 8.49M7.76 16.24a6 6 0 010-8.49" />
        <path d="M19.07 4.93a10 10 0 010 14.14M4.93 19.07a10 10 0 010-14.14" />
      </svg>
    ),
  },
  {
    path: '/settings',
    label: 'Settings',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <circle cx="12" cy="12" r="3" />
        <path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42" />
      </svg>
    ),
  },
  {
    path: '/connect',
    label: 'Connect',
    icon: (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
        <path d="M10 13a5 5 0 007.54.54l3-3a5 5 0 00-7.07-7.07l-1.72 1.71" />
        <path d="M14 11a5 5 0 00-7.54-.54l-3 3a5 5 0 007.07 7.07l1.71-1.71" />
      </svg>
    ),
  },
];

export function Sidebar() {
  const [location, setLocation] = useLocation();

  return (
    <aside style={{
      width: 'var(--sidebar-width)',
      minWidth: 'var(--sidebar-width)',
      height: '100vh',
      position: 'sticky',
      top: 0,
      display: 'flex',
      flexDirection: 'column',
      background: 'var(--bg-1)',
      borderRight: '1px solid var(--border)',
      padding: 'var(--space-lg) 0',
    }}>
      {/* Branding */}
      <div style={{
        padding: '0 var(--space-lg)',
        marginBottom: 'var(--space-xl)',
      }}>
        <div style={{
          fontSize: 15,
          fontWeight: 600,
          fontFamily: 'var(--font-mono)',
          color: 'var(--text-primary)',
          letterSpacing: '-0.02em',
        }}>
          Sage Router
        </div>
        <div style={{
          fontSize: 11,
          fontFamily: 'var(--font-mono)',
          color: 'var(--text-tertiary)',
          marginTop: 2,
        }}>
          v0.1.0
        </div>
      </div>

      {/* Navigation */}
      <nav style={{ flex: 1, padding: '0 var(--space-sm)' }}>
        {navItems.map(item => {
          const active = item.path === '/'
            ? location === '/'
            : location.startsWith(item.path);
          return (
            <button
              key={item.path}
              onClick={() => setLocation(item.path)}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 10,
                width: '100%',
                padding: '7px 10px',
                marginBottom: 2,
                fontSize: 13,
                color: active ? 'var(--text-primary)' : 'var(--text-secondary)',
                background: active ? 'var(--bg-2)' : 'transparent',
                borderRadius: 'var(--radius-md)',
                cursor: 'pointer',
                transition: 'var(--transition-fast)',
                textAlign: 'left',
              }}
            >
              <span style={{ opacity: active ? 1 : 0.6, display: 'flex' }}>{item.icon}</span>
              {item.label}
            </button>
          );
        })}
      </nav>

      {/* Footer */}
      <div style={{
        padding: '0 var(--space-lg)',
        borderTop: '1px solid var(--border)',
        paddingTop: 'var(--space-md)',
        display: 'flex',
        flexDirection: 'column',
        gap: 8,
      }}>
        <div style={{
          display: 'flex',
          alignItems: 'center',
          gap: 6,
          fontSize: 11,
          color: 'var(--text-tertiary)',
          fontFamily: 'var(--font-mono)',
        }}>
          <kbd style={{
            padding: '1px 5px',
            background: 'var(--bg-2)',
            border: '1px solid var(--border)',
            borderRadius: 'var(--radius-sm)',
            fontSize: 10,
          }}>
            {navigator.platform?.includes('Mac') ? '\u2318' : 'Ctrl'}+K
          </kbd>
          <span>Command</span>
        </div>
        <button
          onClick={() => {
            logout().then(() => { authState.value = 'login'; }).catch(() => { authState.value = 'login'; });
          }}
          style={{
            fontSize: 11, color: 'var(--text-tertiary)',
            background: 'none', cursor: 'pointer',
            padding: '4px 0', textAlign: 'left',
          }}
        >
          Sign out
        </button>
      </div>
    </aside>
  );
}
