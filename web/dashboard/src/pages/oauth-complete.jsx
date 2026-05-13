import { useEffect } from 'preact/hooks';
import { useLocation } from 'wouter-preact';

// OAuthCompletePage — destination of the bridge's redirect after a
// successful or failed PKCE flow. Reads ?status and ?conn_id (or ?error)
// from the URL, then bounces back to /providers. The originating tab
// (which kicked off startOAuthFlow) is the one polling status — this
// page just renders feedback and closes itself.
export function OAuthCompletePage() {
  const [, setLocation] = useLocation();

  useEffect(() => {
    // This page is opened in the NEW tab that the bridge redirected to
    // (we called window.open(authorize_url, '_blank') from the modal).
    // `window.opener` therefore points at the dashboard tab. Close this
    // tab so the user is left looking at the dashboard, which is also
    // the tab doing the OAuth status poll. On browsers where
    // window.close() is blocked for non-script-opened windows we fall
    // through to the setTimeout below as a navigation fallback.
    const hasOpener = !!window.opener;
    if (hasOpener) {
      try { window.close(); } catch { /* ignore */ }
    }
    const t = setTimeout(() => setLocation('/providers'), 2200);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const params = new URLSearchParams(window.location.search);
  const status = params.get('status');
  const err = params.get('error');

  return (
    <div style={{
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      minHeight: 'calc(100vh - 0px)', padding: 'var(--space-2xl)',
    }}>
      <div style={{
        background: 'var(--bg-1)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)', padding: 'var(--space-2xl)',
        maxWidth: 480, width: '100%', textAlign: 'center',
      }}>
        {status === 'ok' ? (
          <>
            <div style={{ fontSize: 28, color: 'var(--status-green)', marginBottom: 12 }}>&#10003;</div>
            <h1 style={{ fontSize: 18, fontWeight: 600, marginBottom: 8 }}>Subscription connected</h1>
            <p style={{ fontSize: 13, color: 'var(--text-tertiary)' }}>
              You can close this tab. Returning to the dashboard…
            </p>
          </>
        ) : (
          <>
            <div style={{ fontSize: 28, color: 'var(--status-red)', marginBottom: 12 }}>&#9888;</div>
            <h1 style={{ fontSize: 18, fontWeight: 600, marginBottom: 8 }}>Login did not complete</h1>
            <p style={{ fontSize: 13, color: 'var(--text-tertiary)' }}>
              {err || 'Unknown error.'} Returning to the dashboard…
            </p>
          </>
        )}
      </div>
    </div>
  );
}
