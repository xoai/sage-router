import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import { Router, Route, useLocation } from 'wouter-preact';
import { Sidebar } from './components/sidebar';
import { ToastContainer } from './components/toast';
import { CommandPalette } from './components/command-palette';
import { OverviewPage } from './pages/overview';
import { ProvidersPage } from './pages/providers';
import { ModelsPage } from './pages/models';
import { KeysPage } from './pages/keys';
import { UsagePage } from './pages/usage';
import { SettingsPage } from './pages/settings';
import { ConnectPage } from './pages/connect';
import { RoutingPage } from './pages/routing';
import { LoginPage } from './pages/login';
import { SetupPage } from './pages/setup';
import { OAuthCompletePage } from './pages/oauth-complete';
import { ErrorBoundary } from './ErrorBoundary';
import { authCheck, tokenLogin } from './api/client';

// Auth states: 'loading' | 'login' | 'setup' | 'ready'
export const authState = signal('loading');

function checkAuth() {
  authCheck().then(data => {
    if (!data.authenticated) {
      authState.value = 'login';
    } else if (data.needs_setup) {
      authState.value = 'setup';
    } else {
      authState.value = 'ready';
    }
  }).catch(() => {
    authState.value = 'login';
  });
}

function Dashboard() {
  const [, setLocation] = useLocation();

  return (
    <>
      <Sidebar />
      <main style={{
        flex: 1,
        minHeight: '100vh',
        overflow: 'auto',
        background: 'var(--bg-0)',
      }}>
        {/* Per-route ErrorBoundary (carryover #38). A render-time
            failure in one page renders the reload prompt for that
            page only; the sidebar stays reachable so the user can
            navigate to a healthy page. */}
        <Route path="/" component={() => <ErrorBoundary><OverviewPage /></ErrorBoundary>} />
        <Route path="/providers" component={() => <ErrorBoundary><ProvidersPage /></ErrorBoundary>} />
        <Route path="/models" component={() => <ErrorBoundary><ModelsPage /></ErrorBoundary>} />
        {/* path is BARE — Router base="/dashboard" already prefixes.
            Setting "/dashboard/keys" here would 404 (spec-review M2). */}
        <Route path="/keys" component={() => <ErrorBoundary><KeysPage /></ErrorBoundary>} />
        <Route path="/usage" component={() => <ErrorBoundary><UsagePage /></ErrorBoundary>} />
        <Route path="/routing" component={() => <ErrorBoundary><RoutingPage /></ErrorBoundary>} />
        <Route path="/settings" component={() => <ErrorBoundary><SettingsPage /></ErrorBoundary>} />
        <Route path="/connect" component={() => <ErrorBoundary><ConnectPage /></ErrorBoundary>} />
        <Route path="/oauth-complete" component={() => <ErrorBoundary><OAuthCompletePage /></ErrorBoundary>} />
      </main>
      <ToastContainer />
      <CommandPalette onNavigate={setLocation} />

      <style>{`
        @keyframes pulse {
          0%, 100% { opacity: 1; }
          50% { opacity: 0.5; }
        }
        @keyframes slideIn {
          from { opacity: 0; transform: translateY(8px); }
          to { opacity: 1; transform: translateY(0); }
        }
      `}</style>
    </>
  );
}

export function App() {
  useEffect(() => {
    // Check for one-time setup token in URL
    const params = new URLSearchParams(window.location.search);
    const token = params.get('token');

    if (token) {
      // Clear token from URL without reload
      window.history.replaceState({}, '', window.location.pathname);

      tokenLogin(token).then(() => {
        checkAuth();
      }).catch(() => {
        authState.value = 'login';
      });
    } else {
      checkAuth();
    }

    // Listen for 401 events from API client
    const handleUnauth = () => { authState.value = 'login'; };
    window.addEventListener('sage:unauthorized', handleUnauth);
    return () => window.removeEventListener('sage:unauthorized', handleUnauth);
  }, []);

  if (authState.value === 'loading') {
    return (
      <div style={{
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        minHeight: '100vh', width: '100%', background: 'var(--bg-0)', color: 'var(--text-tertiary)',
        fontSize: 13,
      }}>
        Loading...
      </div>
    );
  }

  if (authState.value === 'login') {
    return <LoginPage onSuccess={checkAuth} />;
  }

  if (authState.value === 'setup') {
    return <SetupPage onSuccess={checkAuth} />;
  }

  return (
    <Router base="/dashboard">
      <Dashboard />
    </Router>
  );
}
