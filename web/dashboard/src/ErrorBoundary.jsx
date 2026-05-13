import { Component } from 'preact';

// ErrorBoundary catches render-time exceptions in its subtree and renders
// a reload prompt instead of letting the error bubble up and white-screen
// the entire dashboard. Carryover #38 — `grep -rn "ErrorBoundary"
// web/dashboard/src` returned 0 before this addition; a single render-time
// JSX bug in /models or /providers would crash the whole app.
//
// Preact's componentDidCatch contract mirrors React's: returning a new
// `error` from getDerivedStateFromError (or setState in componentDidCatch)
// switches the boundary into its error-display mode for the rest of the
// component's lifetime. The Reload button re-mounts the whole app via
// `window.location.reload()` so the boundary resets along with all
// signals and fetched state.
//
// Wrap each top-level route in app.jsx; the boundary's scope is the
// page, so a /models crash leaves /providers reachable via the sidebar.
export class ErrorBoundary extends Component {
  state = { error: null };

  static getDerivedStateFromError(error) {
    return { error };
  }

  componentDidCatch(error, info) {
    // Log to the console so operators can diff the dashboard build's
    // stack trace against the production source map if available. The
    // info object carries componentStack which is useful for narrowing
    // the failure to a specific component.
    // eslint-disable-next-line no-console
    console.error('ErrorBoundary caught:', error, info);
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{
          padding: 'var(--space-2xl)',
          maxWidth: 600,
          margin: '40px auto',
          textAlign: 'center',
          background: 'var(--bg-1)',
          border: '1px solid var(--border)',
          borderRadius: 'var(--radius-lg)',
        }}>
          <h2 style={{ fontSize: 18, marginBottom: 'var(--space-md)' }}>Something went wrong</h2>
          <p style={{ fontSize: 13, color: 'var(--text-tertiary)', marginBottom: 'var(--space-lg)' }}>
            This page hit a render error. The rest of the dashboard is still reachable via the sidebar.
          </p>
          <pre style={{
            fontSize: 11,
            fontFamily: 'var(--font-mono)',
            color: 'var(--status-red)',
            background: 'var(--bg-2)',
            padding: 'var(--space-md)',
            borderRadius: 'var(--radius-sm)',
            textAlign: 'left',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
            marginBottom: 'var(--space-lg)',
          }}>
            {this.state.error.message || String(this.state.error)}
          </pre>
          <button
            onClick={() => window.location.reload()}
            style={{
              padding: '8px 16px',
              fontSize: 13,
              color: 'var(--text-primary)',
              background: 'var(--accent)',
              border: 'none',
              borderRadius: 'var(--radius-sm)',
              cursor: 'pointer',
            }}
          >
            Reload
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
