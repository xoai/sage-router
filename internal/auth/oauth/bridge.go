package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"sage-router/internal/auth/providers"
)

// Default operational constants. Tests can override via NewBridgeForTest.
const (
	defaultFlowTTL       = 10 * time.Minute
	defaultSweepInterval = 60 * time.Second
)

// Health values reported by Health(provider) and the /api/auth/oauth/health
// endpoint.
const (
	HealthAvailable = "available"
	HealthPortBusy  = "port_busy"
	HealthStopped   = "stopped"
)

// BridgeHandler is what the bridge calls when a callback successfully
// completes the OAuth exchange. The handler is responsible for creating
// the Connection row and updating the in-memory ProviderSelector — the
// bridge just performs the OAuth dance and hands off.
//
// Return values:
//   - connectionID is the newly-created Connection's ID, surfaced via
//     Status(state) so the dashboard/CLI can confirm completion.
//   - redirectTo is an optional URL the bridge will 302 the browser to
//     (typically the dashboard's success page). Empty → terminal HTML.
type BridgeHandler interface {
	OnFlowComplete(ctx context.Context, flow *Flow, resp *TokenResponse) (connectionID, redirectTo string, err error)
}

// Bridge owns two localhost HTTP listeners (one per PKCE provider) that
// catch OAuth callbacks. It also holds the in-memory `pendingFlows` map
// keyed by state token.
//
// The Bridge is created with [NewBridge] (production ports 1455 + 53692)
// or [NewBridgeForTest] (ephemeral ports + short TTLs for fast tests).
type Bridge struct {
	handler  BridgeHandler
	listenIP string

	openaiPort    int
	anthropicPort int

	flowTTL       time.Duration
	sweepInterval time.Duration

	mu               sync.RWMutex
	servers          map[string]*http.Server // keyed by provider ID
	listenAddrs      map[string]string       // keyed by provider ID — populated on bind
	health           map[string]string       // keyed by provider ID
	pendingFlows     sync.Map                // state (string) → *flowEntry
	completedFlows   sync.Map                // state (string) → *completion
	sweeperCancel    context.CancelFunc
	sweeperDone      chan struct{}
	stopped          bool
}

// flowEntry wraps a Flow with its arrival time so the sweeper can age it out.
type flowEntry struct {
	flow      *Flow
	expiresAt time.Time
}

// CallbackStatus is what Status(state) returns to a poller (dashboard or CLI).
type CallbackStatus string

const (
	StatusPending  CallbackStatus = "pending"
	StatusComplete CallbackStatus = "complete"
	StatusError    CallbackStatus = "error"
	StatusUnknown  CallbackStatus = "unknown"
)

// StatusInfo is the polling response shape.
type StatusInfo struct {
	Status       CallbackStatus
	ConnectionID string
	Error        string
}

// completion is what we record after the callback runs (success or failure)
// so the dashboard's status-poll endpoint can report the outcome. Same TTL
// as the pendingFlow it replaced — completed entries also need to expire
// so the map doesn't grow unbounded.
type completion struct {
	info      StatusInfo
	expiresAt time.Time
}

// NewBridge constructs a bridge with the spec-mandated ports 1455 (openai)
// and 53692 (anthropic). Production code uses this.
func NewBridge(handler BridgeHandler) *Bridge {
	return &Bridge{
		handler:       handler,
		listenIP:      "127.0.0.1",
		openaiPort:    1455,
		anthropicPort: 53692,
		flowTTL:       defaultFlowTTL,
		sweepInterval: defaultSweepInterval,
		servers:       map[string]*http.Server{},
		listenAddrs:   map[string]string{},
		health:        map[string]string{"openai": HealthStopped, "anthropic": HealthStopped},
	}
}

// NewBridgeForTest returns a bridge configured for in-process tests:
// loopback IP, OS-assigned ports (per provider — fetched from the listener
// after Start), and accelerated flow TTL / sweep interval so tests don't
// sit waiting for the production defaults.
func NewBridgeForTest(handler BridgeHandler) *Bridge {
	b := NewBridge(handler)
	b.openaiPort = 0
	b.anthropicPort = 0
	b.flowTTL = 200 * time.Millisecond
	b.sweepInterval = 50 * time.Millisecond
	return b
}

// Start binds both per-provider listeners and spins up the sweeper. Per-port
// bind failures are non-fatal: the bridge enters degraded mode for the
// affected provider (health → "port_busy"). The function returns nil
// whenever the bridge is usable for at least one provider — only complete
// failure to bind either port AND a non-zero requested-port configuration
// surfaces an error here. Callers should always check Health(provider)
// before steering users at a PKCE flow.
func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return errors.New("bridge: already stopped")
	}
	b.mu.Unlock()

	openaiHealth := b.startPort("openai", b.openaiPort, providers.Providers["openai"].RedirectPath)
	anthropicHealth := b.startPort("anthropic", b.anthropicPort, providers.Providers["anthropic"].RedirectPath)

	// Start the sweeper either way — even with both ports busy, registered
	// flows from a prior incarnation could still time out cleanly.
	sweepCtx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	b.sweeperCancel = cancel
	b.sweeperDone = make(chan struct{})
	b.mu.Unlock()
	go b.runSweeper(sweepCtx)

	// If BOTH ports failed AND the user explicitly requested non-zero
	// ports, there's a meaningful error to surface (operator probably
	// has Codex CLI + Claude Code running at the same time). For ephemeral
	// ports (port=0), failure here is unexpected but not catastrophic.
	if openaiHealth == HealthPortBusy && anthropicHealth == HealthPortBusy {
		slog.Warn("oauth bridge: both ports unavailable",
			"openai_port", b.openaiPort, "anthropic_port", b.anthropicPort)
		// Still return nil — tests + future TryRebind can recover.
	}
	return nil
}

// startPort binds one provider's listener and starts an http.Server on it.
// Updates b.health for the provider and stores the bound listener address
// in b.listenAddrs so tests can construct URLs without knowing the port.
func (b *Bridge) startPort(providerID string, port int, callbackPath string) string {
	addr := fmt.Sprintf("%s:%d", b.listenIP, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		b.mu.Lock()
		b.health[providerID] = HealthPortBusy
		b.mu.Unlock()
		slog.Warn("oauth bridge: bind failed",
			"provider", providerID,
			"addr", addr,
			"err", err,
		)
		return HealthPortBusy
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, b.handleCallback(providerID))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	b.mu.Lock()
	b.servers[providerID] = srv
	b.listenAddrs[providerID] = ln.Addr().String()
	b.health[providerID] = HealthAvailable
	b.mu.Unlock()

	go func() {
		// http.Server.Serve returns http.ErrServerClosed on graceful stop.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("oauth bridge: server stopped unexpectedly",
				"provider", providerID, "err", err)
		}
	}()
	return HealthAvailable
}

// TryRebind attempts to (re)bind a provider's port. Used by the dashboard's
// "retry bind" action after the user stops a conflicting CLI tool. Returns
// the current health for the provider after the attempt.
func (b *Bridge) TryRebind(providerID string) string {
	b.mu.RLock()
	current := b.health[providerID]
	srv := b.servers[providerID]
	b.mu.RUnlock()
	if current == HealthAvailable && srv != nil {
		// Already bound; nothing to do.
		return HealthAvailable
	}

	var port int
	var path string
	switch providerID {
	case "openai":
		port, path = b.openaiPort, providers.Providers["openai"].RedirectPath
	case "anthropic":
		port, path = b.anthropicPort, providers.Providers["anthropic"].RedirectPath
	default:
		return HealthStopped
	}
	return b.startPort(providerID, port, path)
}

// Stop shuts down both listeners and the sweeper. Idempotent.
func (b *Bridge) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	cancel := b.sweeperCancel
	done := b.sweeperDone
	servers := make([]*http.Server, 0, len(b.servers))
	for _, s := range b.servers {
		servers = append(servers, s)
	}
	b.servers = map[string]*http.Server{}
	for k := range b.health {
		b.health[k] = HealthStopped
	}
	b.mu.Unlock()

	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return nil
}

// Health reports the current bind status for providerID. One of
// HealthAvailable / HealthPortBusy / HealthStopped.
func (b *Bridge) Health(providerID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	h, ok := b.health[providerID]
	if !ok {
		return HealthStopped
	}
	return h
}

// CallbackURL returns the redirect_uri the provider should be told to
// redirect to. Useful in tests where the actual port is OS-assigned; in
// production this matches the spec-mandated values.
//
// Host is hardcoded to "localhost" — NOT the listener's bound IP
// ("127.0.0.1"). The OS resolves "localhost" to the loopback address
// via /etc/hosts, so the browser still reaches our listener; but
// upstream OAuth servers (notably OpenAI's Hydra-based Authorization
// Server — see codex-rs/login/src/server.rs:57 "Keep in sync with the
// Codex CLI Hydra redirect URI allow-list") enforce strict redirect_uri
// matching against a registered allow-list (RFC 6749 §3.1.2.3 +
// RFC 8252 §7.3). The Codex CLI public client_id has
// "http://localhost:1455/auth/callback" registered, not the 127.0.0.1
// form — so the URL string we send must be "localhost". Same convention
// applies to the Anthropic Claude Code client.
//
// listenAddrs[providerID] still uses 127.0.0.1 (the bind address);
// we only override the host substring of the URL we emit to the
// provider. Port comes from listenAddrs to support test bridges with
// OS-assigned ports.
func (b *Bridge) CallbackURL(providerID string) string {
	b.mu.RLock()
	addr, hasAddr := b.listenAddrs[providerID]
	b.mu.RUnlock()
	if !hasAddr {
		return ""
	}
	cfg, ok := providers.Providers[providerID]
	if !ok {
		return ""
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Preserve the existing failure-soft contract: empty string
		// when we cannot build a valid URL. Callers already handle "".
		return ""
	}
	return "http://localhost:" + port + cfg.RedirectPath
}

// Register adds a Flow to the pending map with the configured TTL. The
// flow's State must already be set (NewFlow does this).
func (b *Bridge) Register(flow *Flow) {
	b.pendingFlows.Store(flow.State, &flowEntry{
		flow:      flow,
		expiresAt: time.Now().Add(b.flowTTL),
	})
}

// Status reports the current state of the flow identified by `state`. The
// dashboard polls this after starting a flow to detect callback
// completion. Returns StatusUnknown if the state was never seen (e.g.,
// typo), StatusPending while we're waiting for the callback, and
// StatusComplete / StatusError once the callback has run.
func (b *Bridge) Status(state string) StatusInfo {
	if v, ok := b.completedFlows.Load(state); ok {
		entry := v.(*completion)
		if time.Now().Before(entry.expiresAt) {
			return entry.info
		}
		// Expired completion — fall through.
	}
	if _, ok := b.pendingFlows.Load(state); ok {
		return StatusInfo{Status: StatusPending}
	}
	return StatusInfo{Status: StatusUnknown}
}

// recordCompletion stores a terminal status for `state` so the polling
// endpoint can report it. Uses the same TTL as the pending flow.
func (b *Bridge) recordCompletion(state string, info StatusInfo) {
	b.completedFlows.Store(state, &completion{
		info:      info,
		expiresAt: time.Now().Add(b.flowTTL),
	})
}

// invokeHandler is a thin wrapper around BridgeHandler.OnFlowComplete that
// normalizes the result tuple. Kept separate so the callback handler stays
// readable.
func (b *Bridge) invokeHandler(ctx context.Context, flow *Flow, resp *TokenResponse) (redirectTo, connID string, err error) {
	connID, redirectTo, err = b.handler.OnFlowComplete(ctx, flow, resp)
	return redirectTo, connID, err
}

// Consume removes and returns the Flow for a state, or (nil, false) if
// the state is unknown, expired, or already consumed.
func (b *Bridge) Consume(state string) (*Flow, bool) {
	v, ok := b.pendingFlows.LoadAndDelete(state)
	if !ok {
		return nil, false
	}
	entry := v.(*flowEntry)
	if time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.flow, true
}

// handleCallback returns the http.HandlerFunc that runs the post-redirect
// logic for one provider's port. It validates state (Consume), enforces
// that the flow's provider matches the listening port (defense against
// cross-port confusion attacks), runs Exchange, and hands off to the
// BridgeHandler.
//
// The handler wraps the body in `defer recover()` so a panic in the
// injected BridgeHandler (e.g., a nil ProviderSelector during a
// misconfigured deploy) doesn't kill the bridge goroutine for this
// provider until the next rebind.
func (b *Bridge) handleCallback(providerID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("oauth bridge: panic in callback handler",
					"provider", providerID, "panic", rec)
				b.respondError(w, "internal error")
			}
		}()
		q := r.URL.Query()
		state := q.Get("state")
		code := q.Get("code")

		// Generic 400 for any validation failure — never reveal why
		// (state mismatch, expired, wrong provider) so an attacker
		// can't probe the bridge for valid states.
		if state == "" || code == "" {
			b.respondError(w, "missing state or code parameter")
			slog.Warn("oauth bridge: missing query params",
				"provider", providerID, "state_present", state != "", "code_present", code != "")
			return
		}

		flow, ok := b.Consume(state)
		if !ok {
			b.respondError(w, "invalid state")
			slog.Warn("oauth bridge: unknown or expired state",
				"provider", providerID, "state_prefix", safePrefix(state))
			return
		}
		if flow.Provider != providerID {
			b.respondError(w, "invalid state")
			slog.Warn("oauth bridge: cross-port confusion",
				"flow_provider", flow.Provider, "callback_port_provider", providerID)
			return
		}

		// Exchange the code for tokens. The redirect URI we send back to
		// the provider MUST match what we sent at AuthorizeURL time.
		redirectURI := b.CallbackURL(providerID)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		resp, err := flow.Exchange(ctx, code, redirectURI)
		if err != nil {
			b.recordCompletion(state, StatusInfo{Status: StatusError, Error: "token exchange failed"})
			b.respondError(w, "token exchange failed")
			slog.Warn("oauth bridge: exchange failed",
				"provider", providerID, "err", err)
			return
		}

		redirectTo, connID, err := b.invokeHandler(ctx, flow, resp)
		if err != nil {
			b.recordCompletion(state, StatusInfo{Status: StatusError, Error: "handler error"})
			b.respondError(w, "internal error completing flow")
			slog.Warn("oauth bridge: handler error",
				"provider", providerID, "err", err)
			return
		}
		b.recordCompletion(state, StatusInfo{Status: StatusComplete, ConnectionID: connID})

		if redirectTo != "" {
			// Validate the redirect to avoid open-redirect from a
			// compromised handler. We allow any URL the handler chose —
			// it's trusted internal code — but parse-check it first.
			if _, perr := url.Parse(redirectTo); perr != nil {
				b.respondError(w, "internal error")
				slog.Warn("oauth bridge: invalid handler redirect", "err", perr)
				return
			}
			http.Redirect(w, r, redirectTo, http.StatusFound)
			return
		}
		// Terminal page: the popup can close itself; for full-redirect
		// flows the user sees a "you can close this" message.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, terminalSuccessHTML)
	}
}

// respondError writes a generic 400 with no information disclosure. The
// real diagnosis is in the server logs.
func (b *Bridge) respondError(w http.ResponseWriter, _ string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintln(w, "OAuth callback rejected. Please retry from the dashboard or CLI.")
}

// runSweeper periodically removes expired pending AND completed flows.
// Runs until ctx is done; closes sweeperDone on exit so Stop can wait
// for it.
func (b *Bridge) runSweeper(ctx context.Context) {
	defer close(b.sweeperDone)
	ticker := time.NewTicker(b.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			b.pendingFlows.Range(func(key, value any) bool {
				if entry, ok := value.(*flowEntry); ok && now.After(entry.expiresAt) {
					b.pendingFlows.Delete(key)
				}
				return true
			})
			b.completedFlows.Range(func(key, value any) bool {
				if entry, ok := value.(*completion); ok && now.After(entry.expiresAt) {
					b.completedFlows.Delete(key)
				}
				return true
			})
		}
	}
}

// safePrefix returns the first 8 chars of a state token, used in logs so
// operators can correlate without leaking the full random value.
func safePrefix(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

const terminalSuccessHTML = `<!doctype html>
<html>
<head><meta charset="utf-8"><title>Login complete</title></head>
<body style="font-family: system-ui, sans-serif; padding: 2em; text-align: center;">
<h2>Login complete</h2>
<p>Your subscription connection has been added to sage-router.</p>
<p>You can close this window.</p>
<script>setTimeout(function(){window.close();},2000);</script>
</body>
</html>`
