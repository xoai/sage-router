package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/detect"
	"sage-router/internal/catalog"
	"sage-router/internal/config"
	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// ── Auth Routes ──

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	valid := s.deps.Auth.CheckPassword(req.Password)
	if !valid {
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	token, err := s.deps.Auth.GenerateToken(24 * time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	s.deps.Auth.SetAuthCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.deps.Auth.ClearAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("sage-auth")
	if err != nil || cookie.Value == "" {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	info, err := s.deps.Auth.ValidateTokenInfo(cookie.Value)
	if err != nil || !info.Valid {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"needs_setup":   info.Setup || s.deps.Auth.NeedsSetup(),
	})
}

func (s *Server) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "token required")
		return
	}

	if !s.deps.Auth.ValidateSetupToken(token) {
		writeError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}

	// Issue a setup JWT — can only be used to set a password
	jwt, err := s.deps.Auth.GenerateSetupJWT(1 * time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	s.deps.Auth.SetAuthCookie(w, jwt)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "needs_setup": true})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Verify caller has a valid JWT (setup or normal)
	cookie, err := r.Cookie("sage-auth")
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if _, err := s.deps.Auth.ValidateToken(cookie.Value); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}

	// Only allow setup when no password exists
	if !s.deps.Auth.NeedsSetup() {
		writeError(w, http.StatusConflict, "password already set")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	// Hash and store
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	if err := s.deps.Store.SetSetting("password_hash", hash); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save password")
		return
	}
	s.deps.Auth.SetPasswordHash(hash)

	// Issue a normal JWT (not setup)
	jwt, err := s.deps.Auth.GenerateToken(24 * time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	s.deps.Auth.SetAuthCookie(w, jwt)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Connection Routes ──

// legacyRuntimeState derives the legacy connection-state string from the three
// health facets, for the /api/connections `runtime_state` projection (spec §8
// of cycle 20260520-m2-circuit-breaker). The derivation precedence — disabled →
// auth → breaker → active → idle, with the breaker-open split (failure kind
// rate_limit → "cooldown", anything else → "errored") — keeps the dashboard's
// existing `runtime_state` consumer byte-for-byte unchanged by the facet
// migration. The dashboard's own move to render the breaker/auth/lifecycle
// facet fields directly is a tracked post-M2 deferral.
func legacyRuntimeState(breaker provider.BreakerState, authState provider.AuthState, lifecycle provider.LifecycleState, failure provider.FailureKind) string {
	switch {
	case lifecycle == provider.LifecycleDisabled:
		return "disabled"
	case authState == provider.AuthRefreshing:
		return "refreshing"
	case authState == provider.AuthExpired:
		return "auth_expired"
	case breaker == provider.BreakerOpen || breaker == provider.BreakerHalfOpen:
		if failure == provider.FailureRateLimit {
			return "cooldown"
		}
		return "errored"
	case lifecycle == provider.LifecycleActive:
		return "active"
	default:
		return "idle"
	}
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	conns, err := s.deps.Store.ListConnections(store.ConnectionFilter{Provider: provider})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Redact sensitive fields. Subscription connections additionally
	// surface expires_at, account_id (parsed from provider_data),
	// refresh_failures, and last_error so the dashboard can render the
	// AC32 details (badge, expiry countdown, re-authenticate action).
	// Access/refresh tokens and API keys are never echoed.
	//
	// Cycle 20260516-connection-runtime-state: also project the Selector's
	// runtime view (RuntimeState, ModelDenylist, ModelLocks, CooldownUntil)
	// so the dashboard can surface in-memory filter state that diverges from
	// the DB row's `state`. Without this projection, a connection appears
	// "green" in UI while silently refusing specific models — see plan.md.
	type safeConn struct {
		ID              string     `json:"id"`
		Provider        string     `json:"provider"`
		Name            string     `json:"name"`
		AuthType        string     `json:"auth_type"`
		Priority        int        `json:"priority"`
		State           string     `json:"state"`
		ExpiresAt       *time.Time `json:"expires_at,omitempty"`
		AccountID       string     `json:"account_id,omitempty"`
		RefreshFailures int        `json:"refresh_failures,omitempty"`
		LastError       string     `json:"last_error,omitempty"`
		// Selector runtime view (cycle 20260516-connection-runtime-state).
		// Populated from provider.Connection when registered in the Selector;
		// all four use omitempty so an unregistered connection (e.g., between
		// handleCreateConnection insert and Selector.Register) renders as today.
		RuntimeState  string               `json:"runtime_state,omitempty"`  // legacy state derived from the facets — may differ from State
		ModelDenylist map[string]time.Time `json:"model_denylist,omitempty"` // model → expiry (1h after 401/403)
		ModelLocks    map[string]time.Time `json:"model_locks,omitempty"`    // model → expiry (per-429 cooldown)
		CooldownUntil *time.Time           `json:"cooldown_until,omitempty"` // nil when not in cooldown
		// Three-facet health view (cycle 20260520-m2-circuit-breaker, spec §8):
		// breaker = transient-health circuit breaker; auth = credential
		// validity; lifecycle = request lifecycle. omitempty — same contract as
		// the runtime fields above (omitted for an unregistered connection).
		Breaker   string    `json:"breaker,omitempty"`
		Auth      string    `json:"auth,omitempty"`
		Lifecycle string    `json:"lifecycle,omitempty"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	safe := make([]safeConn, 0, len(conns))
	for _, c := range conns {
		sc := safeConn{
			ID:              c.ID,
			Provider:        c.Provider,
			Name:            c.Name,
			AuthType:        c.AuthType,
			Priority:        c.Priority,
			State:           c.State,
			ExpiresAt:       c.ExpiresAt,
			RefreshFailures: c.RefreshFailures,
			CreatedAt:       c.CreatedAt,
			UpdatedAt:       c.UpdatedAt,
		}
		// account_id lives inside provider_data, populated by JWT
		// extraction (OpenAI) or import parsers (Anthropic email,
		// GitHub user). Fail-soft on parse: a row without account_id
		// just renders without one.
		if len(c.ProviderData) > 0 {
			var pd struct {
				AccountID string `json:"account_id"`
			}
			_ = json.Unmarshal(c.ProviderData, &pd)
			sc.AccountID = pd.AccountID
		}
		// last_error comes from the in-memory provider.Connection — it
		// rotates on every state transition, so the DB doesn't hold it.
		// Only expose it when the connection is in a state where the
		// user might need to act (errored / disabled / auth_expired);
		// otherwise it's noise. (Kept gated per fix-plan-review fold:
		// OpenBreaker doesn't clear lastError, so a connection in cooldown
		// would surface stale auth errors. Rate-limit context is conveyed
		// via the structured ModelLocks/CooldownUntil fields below.)
		//
		// Cycle 20260516-connection-runtime-state: also project the Selector's
		// runtime view so the dashboard can detect DB-vs-runtime state drift
		// + show per-model filter state that would otherwise be invisible.
		if pc := s.deps.ProviderSelector.ConnectionByID(c.ID); pc != nil {
			if err := pc.LastError(); err != nil {
				switch c.State {
				case "errored", "disabled", "auth_expired":
					sc.LastError = err.Error()
				}
			}
			// Read the three facets into locals so the derived runtime_state
			// stays consistent with the breaker/auth/lifecycle fields shown
			// alongside it in this same response (spec §8).
			breaker := pc.Breaker()
			authState := pc.Auth()
			lifecycle := pc.Lifecycle()
			sc.Breaker = string(breaker)
			sc.Auth = string(authState)
			sc.Lifecycle = string(lifecycle)
			sc.RuntimeState = legacyRuntimeState(breaker, authState, lifecycle, pc.FailureKind())
			if dn := pc.ModelDenylistSnapshot(); len(dn) > 0 {
				sc.ModelDenylist = dn
			}
			if lk := pc.ModelLocksSnapshot(); len(lk) > 0 {
				sc.ModelLocks = lk
			}
			if cu := pc.CooldownUntil(); !cu.IsZero() {
				sc.CooldownUntil = &cu
			}
		}
		safe = append(safe, sc)
	}
	writeJSON(w, http.StatusOK, safe)
}

func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	var conn store.Connection
	if err := json.NewDecoder(r.Body).Decode(&conn); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if conn.Provider == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}

	conn.ID = generateRequestID()
	conn.State = "idle"
	conn.CreatedAt = time.Now()
	conn.UpdatedAt = time.Now()

	// For auto_detect connections, read credentials from disk now and
	// convert to a standard subscription connection (encrypted token
	// storage + refresh-loop rotation from then on). The auto_detect
	// auth_type is a transient signal from the frontend: "please read
	// my CLI's auth file instead of asking me for tokens." It never
	// persists to the DB — the conversion below changes auth_type to
	// AuthTypeSubscription so the rest of the system treats the row
	// like any other imported subscription.
	if conn.AuthType == "auto_detect" && conn.Provider == "anthropic" {
		result, creds := detect.DetectClaude()
		if !result.Found || creds == nil {
			writeError(w, http.StatusBadRequest, "Claude Code credentials not found on this machine")
			return
		}
		if result.Expired {
			writeError(w, http.StatusBadRequest, "Claude Code credentials are expired — restart Claude Code to refresh")
			return
		}
		conn.AccessToken = creds.AccessToken
		conn.RefreshToken = creds.RefreshToken
		conn.AuthType = auth.AuthTypeSubscription // canonical: we now have a real OAuth token
	} else if conn.AuthType == "auto_detect" && conn.Provider == "openai" {
		// Parallel arm for Codex CLI auto-detect (initiative
		// 20260513-openai-autodetect). Mirrors the anthropic arm
		// byte-for-byte in shape; only the detect source + error
		// messages differ. Adding a third provider in the future
		// remains additive at this block.
		result, creds := detect.DetectCodex()
		if !result.Found || creds == nil {
			writeError(w, http.StatusBadRequest, "Codex CLI credentials not found on this machine")
			return
		}
		if result.Expired {
			writeError(w, http.StatusBadRequest, "Codex CLI credentials are expired — restart Codex CLI to refresh")
			return
		}
		conn.AccessToken = creds.AccessToken
		conn.RefreshToken = creds.RefreshToken
		conn.AuthType = auth.AuthTypeSubscription
		// Cycle 20260517-provider-auth-variants M2.6.2: token-exchange
		// auto-chain REMOVED (was M2b.4-extend wrong-path). CodexSubscriptionExecutor
		// uses the PKCE access_token directly against
		// chatgpt.com/backend-api/codex/responses; no exchange step.
		// Stash chatgpt_account_id so AuthStore.GetCredential surfaces
		// it as cred.AccountID → ExtraHeaders → ChatGPT-Account-ID header
		// on every downstream /v1/* call (executor + discovery listers).
		// Matches the providerData JSON shape at internal/auth/store.go:54-59.
		if creds.AccountID != "" {
			if raw, mErr := json.Marshal(map[string]string{"account_id": creds.AccountID}); mErr == nil {
				conn.ProviderData = raw
			}
		}
	}

	if err := s.deps.Store.CreateConnection(&conn); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Register with selector
	provConn := provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType)
	s.deps.ProviderSelector.Register(provConn)

	// Models Discovery M2.4 — fire-and-forget discovery for the new
	// connection. Surfaces models to catalog_models within seconds of
	// "add connection" so the dashboard's /api/catalog/models reflects
	// reality without waiting for the 24h ticker.
	//
	// AC13: triggers for apikey connections on discovery_enabled providers.
	// AC19: subscription-auth connections only trigger when the
	// provider's subscription_discoverable=true (static M3 allowlist
	// remains authoritative for providers that opt out).
	if s.deps.Discovery != nil && s.deps.CatalogStore != nil {
		go s.triggerOnCreateDiscovery(conn)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"id": conn.ID})
}

// triggerOnCreateDiscovery runs the model-discovery flow for a
// newly-created connection. Called as a goroutine — never blocks the
// HTTP response.
//
// Discovery is skipped when:
//   - provider has discovery_enabled=false (e.g., github-copilot)
//   - AuthType=subscription but subscription_discoverable=false
//   - no lister is registered for the provider (returns gracefully)
//   - the provider is unknown to KnownProviders (no BaseURL)
//
// Bypasses backoff per ADR-2 §"On-connection-create discovery
// bypasses backoff once" — new credentials are reason enough to
// retry. (Backoff-state checks against the freshly-written ProviderMeta
// don't apply: the user took an explicit action.)
func (s *Server) triggerOnCreateDiscovery(conn store.Connection) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	meta, err := s.deps.CatalogStore.GetProviderMeta(ctx, conn.Provider)
	if err != nil {
		slog.Warn("on-create discovery: read ProviderMeta failed",
			"provider", conn.Provider, "err", err)
		return
	}
	if meta == nil || !meta.DiscoveryEnabled {
		return
	}
	if conn.AuthType == auth.AuthTypeSubscription && !meta.SubscriptionDiscoverable {
		return
	}

	creds := s.buildListerCredentials(conn)
	if creds.BaseURL == "" {
		// Unknown provider — we have no idea where to send the request.
		// Shouldn't happen if the connection passed validation, but be
		// defensive.
		slog.Warn("on-create discovery: no BaseURL for provider", "provider", conn.Provider)
		return
	}

	// Dispatch through DiscoveryListerKey: most provider/auth pairs use
	// the provider's own lister; subscription openai routes through
	// "openai@openrouter-mirror" because ChatGPT subscription tokens
	// can't read api.openai.com/v1/models (fix 20260514-openrouter-fallback).
	listerKey := catalog.DiscoveryListerKey(conn.Provider, conn.AuthType)
	res := s.deps.Discovery.DiscoverProviderWithLister(ctx, conn.Provider, listerKey, creds)
	if res.Err != nil {
		slog.Warn("on-create discovery: lister error",
			"provider", conn.Provider, "lister_key", listerKey, "connection_id", conn.ID, "err", res.Err)
	}
}

// buildListerCredentials translates a store.Connection + the static
// provider definition into a catalog.ListerCredentials. Keeps the
// catalog package decoupled from store.Connection / config types.
//
// For subscription connections, also queries AuthStore to thread
// provider-specific ExtraHeaders (e.g., ChatGPT-Account-ID for openai).
// This mirrors the request-time path at routes_v1.go:1031-1035, so
// discovery sees the same auth shape as chat completions.
func (s *Server) buildListerCredentials(conn store.Connection) catalog.ListerCredentials {
	provDef, ok := config.KnownProviders[conn.Provider]
	if !ok {
		return catalog.ListerCredentials{}
	}
	creds := catalog.ListerCredentials{
		BaseURL:     provDef.BaseURL,
		APIKey:      conn.APIKey,
		AccessToken: conn.AccessToken,
	}
	if conn.AuthType == auth.AuthTypeSubscription && s.deps.AuthStore != nil {
		if cred, err := s.deps.AuthStore.GetCredential(conn.ID); err == nil && cred != nil {
			creds.ExtraHeaders = cred.ExtraHeaders()
		}
	}
	return creds
}

// triggerOnNotFoundDiscovery is the upstream-404 sibling of
// triggerOnCreateDiscovery (M2.7, AC16). When an executor returns
// "model not found," the catalog is likely stale — refresh it so the
// next request and the dashboard see the up-to-date list. Fire-and-
// forget — the 30s budget is independent of the client's already-
// returned 404 response.
//
// Gates (mirrored from M2.4 for parity):
//  1. `SubscriptionDiscoverable=false` for subscription-auth connections
//     (AC19) — the static M3 allowlist stays authoritative for providers
//     that opted out of OAuth-token-driven discovery.
//  2. Persistent backoff + 5-min debounce — owned by
//     `DiscoveryRunner.TryDiscoverOnNotFound`; the server-side hook
//     only translates `ConnectionInfo` into `ListerCredentials`.
func (s *Server) triggerOnNotFoundDiscovery(providerID string, conn *ConnectionInfo) {
	if conn == nil || conn.Credentials == nil {
		return
	}
	// Defensive nil checks (M2.7 review m2-r3-2). The hook site in
	// routes_v1.go already guards on these, but this helper might be
	// called from a future code path that doesn't — keep the contract
	// local so a missing-wiring bug surfaces as a no-op, not a panic.
	if s.deps.CatalogStore == nil || s.deps.Discovery == nil {
		return
	}
	provDef, ok := config.KnownProviders[providerID]
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// AC19 parity with M2.4: subscription tokens may only be used as
	// discovery credentials on providers that explicitly opt in. Without
	// this gate the on-404 hook would silently undo M2.4's protection
	// every time the upstream returned 404 to a subscription connection
	// on `gemini` / `openrouter` / `ollama` (all seeded
	// SubscriptionDiscoverable=false). The duplicate ProviderMeta read
	// vs. TryDiscoverOnNotFound is intentional — the gate is an auth
	// concern that the catalog package shouldn't know about.
	if conn.Credentials.AuthType == auth.AuthTypeSubscription {
		meta, err := s.deps.CatalogStore.GetProviderMeta(ctx, providerID)
		if err != nil {
			slog.Warn("on-404 discovery: read ProviderMeta failed",
				"provider", providerID, "err", err)
			return
		}
		if meta == nil || !meta.SubscriptionDiscoverable {
			slog.Debug("on-404 discovery: subscription_discoverable=false; skipping",
				"provider", providerID, "connection_id", conn.ID)
			return
		}
	}

	creds := catalog.ListerCredentials{
		BaseURL:      provDef.BaseURL,
		APIKey:       conn.Credentials.APIKey,
		AccessToken:  conn.Credentials.AccessToken,
		ExtraHeaders: conn.Credentials.ExtraHeaders, // pre-populated at routes_v1.go:1031-1035 for subscription connections
	}

	// Dispatch through DiscoveryListerKey for the same reason as the
	// on-create path. See fix 20260514-openrouter-fallback.
	listerKey := catalog.DiscoveryListerKey(providerID, conn.Credentials.AuthType)
	res := s.deps.Discovery.TryDiscoverOnNotFoundWithLister(ctx, providerID, listerKey, creds)
	switch {
	case res.Skipped:
		// Gated by backoff or debounce — expected during 404 storms.
		// Trace-level only to keep production logs quiet.
		slog.Debug("on-404 discovery: skipped",
			"provider", providerID, "connection_id", conn.ID)
	case res.Err != nil:
		slog.Warn("on-404 discovery: lister error",
			"provider", providerID, "connection_id", conn.ID, "err", res.Err)
	default:
		slog.Info("on-404 discovery: completed",
			"provider", providerID, "connection_id", conn.ID,
			"count", res.Count, "empty", res.Empty)
	}
}

func (s *Server) handleUpdateConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updates["updated_at"] = time.Now()

	if err := s.deps.Store.UpdateConnection(id, updates); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Store.DeleteConnection(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.deps.ProviderSelector.Remove(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	conn, err := s.deps.Store.GetConnection(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "connection not found")
		return
	}

	// Determine provider base URL and probe endpoint
	provDef, hasDef := config.KnownProviders[conn.Provider]
	if !hasDef {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "unknown provider"})
		return
	}

	// Build probe request based on provider type
	var probeURL, method string
	var probeBody io.Reader
	probeHeaders := map[string]string{}

	switch conn.Provider {
	case "anthropic":
		// Anthropic: POST /v1/messages with max_tokens=1
		probeURL = provDef.BaseURL + "/v1/messages"
		method = "POST"
		probeBody = strings.NewReader(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
		probeHeaders["Content-Type"] = "application/json"
		probeHeaders["anthropic-version"] = "2023-06-01"
		if conn.AuthType == "auto_detect" {
			_, creds := detect.DetectClaude()
			if creds != nil && creds.AccessToken != "" {
				probeHeaders["Authorization"] = "Bearer " + creds.AccessToken
			}
		} else if conn.APIKey != "" {
			probeHeaders["x-api-key"] = conn.APIKey
		}
	case "gemini":
		// Gemini: GET /models
		probeURL = provDef.BaseURL + "/models"
		method = "GET"
		if conn.APIKey != "" {
			probeURL += "?key=" + conn.APIKey
		}
	default:
		// OpenAI-compatible: GET /models
		probeURL = provDef.BaseURL + "/models"
		method = "GET"
		if conn.APIKey != "" {
			probeHeaders["Authorization"] = "Bearer " + conn.APIKey
		} else if conn.AccessToken != "" {
			probeHeaders["Authorization"] = "Bearer " + conn.AccessToken
		}
	}

	// Execute probe with 10s timeout
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, probeURL, probeBody)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for k, v := range probeHeaders {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "latency_ms": latency.Milliseconds()})
		return
	}
	defer resp.Body.Close()

	// Read first 512 bytes of response for diagnostics
	bodyBytes := make([]byte, 512)
	n, _ := resp.Body.Read(bodyBytes)
	bodySnippet := string(bodyBytes[:n])

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"provider":   conn.Provider,
			"status":     resp.StatusCode,
			"latency_ms": latency.Milliseconds(),
		})
	} else {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"provider":   conn.Provider,
			"status":     resp.StatusCode,
			"error":      bodySnippet,
			"latency_ms": latency.Milliseconds(),
		})
	}
}

// ── Combo Routes ──

func (s *Server) handleListCombos(w http.ResponseWriter, r *http.Request) {
	combos, err := s.deps.Store.ListCombos()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, combos)
}

func (s *Server) handleCreateCombo(w http.ResponseWriter, r *http.Request) {
	var combo store.Combo
	if err := json.NewDecoder(r.Body).Decode(&combo); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	combo.ID = generateRequestID()
	combo.CreatedAt = time.Now()

	if err := s.deps.Store.CreateCombo(&combo); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": combo.ID})
}

func (s *Server) handleUpdateCombo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var combo store.Combo
	if err := json.NewDecoder(r.Body).Decode(&combo); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.deps.Store.UpdateCombo(id, &combo); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteCombo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Store.DeleteCombo(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Alias Routes ──

func (s *Server) handleListAliases(w http.ResponseWriter, r *http.Request) {
	aliases, err := s.deps.Store.ListAliases()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, aliases)
}

func (s *Server) handleSetAlias(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Alias  string `json:"alias"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.deps.Store.SetAlias(req.Alias, req.Target); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteAlias(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deps.Store.DeleteAlias(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── API Key Routes ──

// handleListAPIKeys returns a paginated + filtered view of API keys.
// Cycle 20260516-keys-management-redesign breaking contract change:
// response shape is {items, total, limit, offset} (not bare []APIKey).
//
// Query params (all optional):
//
//	limit       integer 1..200, default 25
//	offset      integer ≥0,    default 0
//	search      substring on Name (case-insensitive, LOWER(name) LIKE LOWER(?))
//	routing     "" | "default" | "fast" | "balanced" | "cheap" | "best"
//	has_budget  "true" | "false" (strict — no other values; ParseBool rejected)
//
// Malformed values return HTTP 400 with the canonical envelope
// {"error":{"message":"..."}} per the cycle 20260515 contract.
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseIntQueryParam(r, w, "limit", 25, 1, 200)
	if !ok {
		return
	}
	offset, ok := parseIntQueryParam(r, w, "offset", 0, 0, -1) // -1 sentinel = no upper bound
	if !ok {
		return
	}

	var hasBudget *bool
	switch raw := r.URL.Query().Get("has_budget"); raw {
	case "":
		// no filter
	case "true":
		v := true
		hasBudget = &v
	case "false":
		v := false
		hasBudget = &v
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q value: must be 'true' or 'false' (got %q)", "has_budget", raw))
		return
	}

	// Sort params — strict whitelist for AC-A2 (sortable headers).
	// SQL identifier injection is the risk if these pass through to
	// ORDER BY without validation; the switch is the boundary guard.
	sortField, sortDir, ok := parseSortParam(w, r.URL.Query().Get("sort"), r.URL.Query().Get("dir"))
	if !ok {
		return
	}

	filter := store.APIKeyFilter{
		Search:    r.URL.Query().Get("search"),
		Routing:   r.URL.Query().Get("routing"),
		HasBudget: hasBudget,
		Limit:     limit,
		Offset:    offset,
		SortField: sortField,
		SortDir:   sortDir,
	}

	page, err := s.deps.Store.ListAPIKeysPaged(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// parseSortParam validates the (sort, dir) pair against a strict
// whitelist before letting either flow into SQL ORDER BY. Returns
// ("", "", true) when both params are absent (caller uses the
// historical default created_at DESC). Returns ("", "", false) after
// writing a 400 envelope on invalid input.
//
// AC-A2 of 20260516-keys-management-redesign post-review revision.
// Strict whitelist because SortField/SortDir flow into raw SQL at
// sqlite.go (the only acceptable injection guard is rejection here).
func parseSortParam(w http.ResponseWriter, sort, dir string) (string, string, bool) {
	// Both absent — fall back to default ordering at the SQL layer.
	if sort == "" && dir == "" {
		return "", "", true
	}
	switch sort {
	case "name", "created_at":
		// ok
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q value: must be 'name' or 'created_at' (got %q)", "sort", sort))
		return "", "", false
	}
	switch dir {
	case "", "asc", "desc":
		// ok; empty dir defaults to ASC at the SQL layer when paired with
		// an explicit sort field. (Empty dir + empty sort is handled above.)
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q value: must be 'asc' or 'desc' (got %q)", "dir", dir))
		return "", "", false
	}
	return sort, dir, true
}

// parseIntQueryParam extracts a non-negative integer from a query
// parameter, with optional bounds-checking. minV is the minimum allowed
// value (inclusive); maxV is the maximum (inclusive) — pass -1 for no
// upper bound. Returns (defaultV, true) when the param is absent.
// Mirrors parseTimeQueryParam from cycle 20260515-chat-routing-fix.
func parseIntQueryParam(r *http.Request, w http.ResponseWriter, name string, defaultV, minV, maxV int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return defaultV, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q: must be an integer (got %q)", name, raw))
		return 0, false
	}
	if n < minV {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q: must be ≥ %d (got %d)", name, minV, n))
		return 0, false
	}
	if maxV >= 0 && n > maxV {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q: must be ≤ %d (got %d)", name, maxV, n))
		return 0, false
	}
	return n, true
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name               string  `json:"name"`
		BudgetMonthly      float64 `json:"budget_monthly"`
		BudgetHardLimit    bool    `json:"budget_hard_limit"`
		AllowedModels      string  `json:"allowed_models"`
		RateLimitRPM       int     `json:"rate_limit_rpm"`
		RoutingStrategy    string  `json:"routing_strategy"`
		CompressionEnabled bool    `json:"compression_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	plainKey, keyHash, prefix, err := s.deps.Auth.GenerateAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.AllowedModels == "" {
		req.AllowedModels = "*"
	}

	key := &store.APIKey{
		ID:                 generateRequestID(),
		Name:               req.Name,
		KeyHash:            keyHash,
		Prefix:             prefix,
		BudgetMonthly:      req.BudgetMonthly,
		BudgetHardLimit:    req.BudgetHardLimit,
		AllowedModels:      req.AllowedModels,
		RateLimitRPM:       req.RateLimitRPM,
		RoutingStrategy:    req.RoutingStrategy,
		CompressionEnabled: req.CompressionEnabled,
		CreatedAt:          time.Now(),
	}

	if err := s.deps.Store.CreateAPIKey(key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":   key.ID,
		"key":  plainKey,
		"name": key.Name,
	})
}

func (s *Server) handleUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.deps.Store.UpdateAPIKey(id, req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Store.DeleteAPIKey(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Settings Routes ──

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.deps.Store.AllSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	var settings map[string]string
	if err := json.Unmarshal(body, &settings); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	for k, v := range settings {
		if err := s.deps.Store.SetSetting(k, v); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Usage Routes ──

// parseTimeQueryParam extracts an RFC3339 timestamp from a query
// parameter. Returns (zero, true) when the param is absent (no filter).
// Returns (zero, false) and writes a 400 envelope when the value is
// present but malformed — RFC3339 strictness is intentional so the
// dashboard's toISOString() output (always Z-suffixed) is the only
// valid form. AC-C3 + AC-C4 of 20260515-cost-savings-display.
func parseTimeQueryParam(r *http.Request, w http.ResponseWriter, name string) (time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %q timestamp: must be RFC3339 (got %q)", name, raw))
		return time.Time{}, false
	}
	return t, true
}

func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	from, ok := parseTimeQueryParam(r, w, "from")
	if !ok {
		return
	}
	to, ok := parseTimeQueryParam(r, w, "to")
	if !ok {
		return
	}
	filter := store.UsageFilter{
		Provider:  r.URL.Query().Get("provider"),
		Model:     r.URL.Query().Get("model"),
		APIKeyIDs: r.URL.Query()["api_key_id"],
		From:      from,
		To:        to,
		Limit:     limit,
	}

	entries, err := s.deps.Store.QueryUsage(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Enrich each entry with EstimatedAPICost — the would-have-been-API
	// cost computed at query time via the current pricing table.
	// Mirrors the query-time pattern used for SubscriptionSavings in
	// handleGetUsageSummary below (lines ~843-861). For apikey rows
	// this approximately equals Cost (modulo pricing changes since the
	// row was written); for subscription rows it surfaces the savings
	// value the dashboard renders parenthetically. AC-B3 of
	// 20260515-cost-savings-display.
	if s.deps.Catalog != nil {
		for i := range entries {
			entries[i].EstimatedAPICost = s.deps.Catalog.EstimateCost(
				entries[i].Provider, entries[i].Model,
				catalog.TokenBreakdown{
					Input:        entries[i].InputTokens,
					Output:       entries[i].OutputTokens,
					CacheReadIn:  entries[i].CacheReadTokens,
					CacheWriteIn: entries[i].CacheWriteTokens,
				},
			)
		}
	}

	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleGetUsageSummary(w http.ResponseWriter, r *http.Request) {
	from, ok := parseTimeQueryParam(r, w, "from")
	if !ok {
		return
	}
	to, ok := parseTimeQueryParam(r, w, "to")
	if !ok {
		return
	}
	filter := store.UsageFilter{
		Provider:  r.URL.Query().Get("provider"),
		APIKeyIDs: r.URL.Query()["api_key_id"],
		From:      from,
		To:        to,
	}
	summary, err := s.deps.Store.UsageSummary(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Enrich with SubscriptionSavings — sum of would-have-been API cost
	// across subscription-served rows, using the current pricing table.
	// Computed at query time so pricing-table updates retroactively
	// reflect in reported savings (intentional per spec; documented in
	// the dashboard tooltip in 3.5c).
	groups, err := s.deps.Store.SubscriptionUsageGroups(filter)
	if err != nil {
		// Non-fatal — log and continue without savings.
		slog.Warn("usage summary: subscription groups query failed", "err", err)
	} else if s.deps.Catalog != nil {
		// Models Discovery M1.9b (LOAD-BEARING for AC25 + AC25b).
		// EstimateCost now flows cache_read + cache_write tokens
		// through the catalog's per-row Pricing, so subscription
		// savings reflect cache pricing whenever the underlying row
		// has non-zero CacheRead/CacheWrite (Anthropic Input*1.25 etc).
		// Without the catalog wired (e.g., partial-wired tests),
		// savings default to 0 — explicit fallback per RC1.
		var savings float64
		for _, g := range groups {
			savings += s.deps.Catalog.EstimateCost(g.Provider, g.Model, catalog.TokenBreakdown{
				Input:        g.InputTokens,
				Output:       g.OutputTokens,
				CacheReadIn:  g.CacheReadTokens,
				CacheWriteIn: g.CacheWriteTokens,
			})
		}
		summary.SubscriptionSavings = savings
	}

	writeJSON(w, http.StatusOK, summary)
}

// ── Provider & Model Catalog Routes ──

// providerListEntry is the per-provider row shape for /api/providers.
// Models Discovery M2.12 (AC22) enriched the static config.ProviderDef
// with discovery-loop state. Per AC9's M2.12 revision the change is
// **additive**: every M1 field is preserved, new fields are appended.
// Clients ignoring unknown fields continue to work. The
// /api/catalog/providers endpoint (M2.10) carries the same meta but
// in a flat array; this merged view powers the existing providers.jsx
// page without forcing it to do client-side joins.
//
// Field design notes:
//   - `discovery_enabled`, `subscription_discoverable`, and `backoff_step`
//     are NOT `omitempty`: their zero values (`false`, `0`) are
//     load-bearing — `discovery_enabled=false` for github-copilot is the
//     signal the dashboard renders "discovery off" off. Production
//     always has a `catalog_provider_meta` row (seeded by `SeedProviderMeta`
//     in M1.11), so the "no meta row" fallback path is a test-rig case.
//   - `last_discovered_at` / `last_discovery_error` / `next_discovery_after`
//     ARE `omitempty`: a zero timestamp and empty error string aren't
//     meaningful to surface to operators; the dashboard treats their
//     absence as "no event yet."
//   - Embedding `config.ProviderDef` anonymously promotes its JSON
//     fields to the top level. If `ProviderDef` ever gains a field
//     whose name collides with a meta field (e.g., a future
//     `BackoffStep`), `encoding/json` would silently drop one per
//     embedding rules. Flatten the struct if that risk materialises.
type providerListEntry struct {
	config.ProviderDef
	DiscoveryEnabled         bool   `json:"discovery_enabled"`
	SubscriptionDiscoverable bool   `json:"subscription_discoverable"`
	LastDiscoveredAt         string `json:"last_discovered_at,omitempty"`
	LastDiscoveryError       string `json:"last_discovery_error,omitempty"`
	BackoffStep              int    `json:"backoff_step"`
	NextDiscoveryAfter       string `json:"next_discovery_after,omitempty"`
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	// Snapshot ProviderMeta into a lookup map. A nil CatalogStore (test
	// wiring without M2 deps) leaves every entry with zero-value meta
	// fields. The bool/int fields will still serialise (no omitempty
	// on them — see providerListEntry field notes); the dashboard's
	// "no meta" path expects false/0 to mean "no data yet" and the
	// production seed (M1.11) ensures every provider has a real row.
	metas := map[string]catalog.ProviderMeta{}
	if s.deps.CatalogStore != nil {
		rows, err := s.deps.CatalogStore.ListProviderMetas(r.Context())
		if err == nil {
			for _, m := range rows {
				metas[m.Provider] = m
			}
		}
	}

	out := make(map[string]providerListEntry, len(config.KnownProviders))
	for id, def := range config.KnownProviders {
		entry := providerListEntry{ProviderDef: def}
		if m, ok := metas[id]; ok {
			entry.DiscoveryEnabled = m.DiscoveryEnabled
			entry.SubscriptionDiscoverable = m.SubscriptionDiscoverable
			entry.LastDiscoveryError = m.LastDiscoveryError
			entry.BackoffStep = m.BackoffStep
			if !m.LastDiscoveredAt.IsZero() {
				entry.LastDiscoveredAt = m.LastDiscoveredAt.UTC().Format(time.RFC3339)
			}
			if !m.NextDiscoveryAfter.IsZero() {
				entry.NextDiscoveryAfter = m.NextDiscoveryAfter.UTC().Format(time.RFC3339)
			}
		}
		out[id] = entry
	}
	// encoding/json serialises maps with sorted keys (Go ≥1.12), so the
	// /api/providers payload is deterministic across calls without an
	// explicit ordering step. Dashboard consumers and snapshot-style
	// tests can rely on stable key order. If the serialisation library
	// ever changes, this convention should be promoted to an explicit
	// sort before writeJSON.
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListModelCatalog(w http.ResponseWriter, r *http.Request) {
	// Return models from active connections (not the static catalog)
	connections, _ := s.deps.Store.ListConnections(store.ConnectionFilter{})

	// Collect active provider IDs
	activeProviders := map[string]bool{}
	for _, conn := range connections {
		if conn.State != "disabled" {
			activeProviders[conn.Provider] = true
		}
	}

	// Models Discovery M1.9b — iterate the runtime catalog instead of
	// the static model map. The JSON shape (modelEntry) is preserved
	// byte-for-byte to honor AC9's backward-compat contract; only the
	// iteration source changes.
	type modelEntry struct {
		ID          string  `json:"id"`
		Provider    string  `json:"provider"`
		DisplayName string  `json:"display_name"`
		InputPrice  float64 `json:"input_price,omitempty"`
		OutputPrice float64 `json:"output_price,omitempty"`
	}
	var models []modelEntry
	if s.deps.Catalog != nil {
		for _, m := range s.deps.Catalog.ListProvider("") {
			if activeProviders[m.Provider] {
				models = append(models, modelEntry{
					ID:          m.Provider + "/" + m.ModelID,
					Provider:    m.Provider,
					DisplayName: m.DisplayName,
					InputPrice:  m.Pricing.Input,
					OutputPrice: m.Pricing.Output,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, models)
}

// ── Catalog Routes (Models Discovery M2.10) ──
//
// The new /api/catalog/* endpoints expose the runtime catalog
// (catalog_models, catalog_pricing, catalog_provider_meta) so the
// dashboard can render the discovery loop's effects + edit pricing
// overrides. Distinct from the M1 backward-compat /api/models
// (active-connections filter) and /api/providers (static
// KnownProviders dump):
//
//   - /api/catalog/models       — every catalog row, with source badge
//   - /api/catalog/models/{provider} — same, filtered to one provider
//   - PUT /api/catalog/pricing/{provider}/{model_id...} — user override
//   - DELETE same — remove the user override
//   - /api/catalog/providers    — catalog_provider_meta rows
//
// Path semantics: `model_id` is a catch-all (Go 1.22+ `{name...}`)
// because OpenRouter qualifies IDs as `<vendor>/<model>` (e.g.
// `anthropic/claude-sonnet-4`). Two-segment IDs must round-trip the
// slash.

// catalogModelView is the JSON shape returned by /api/catalog/models.
// Carries every field the dashboard surfaces: source badge,
// capabilities, pricing, and the row's `updated_at` timestamp so
// operators can see when discovery / OpenRouter / a manual edit last
// touched it (AC21 — added after M2.11 review MAJOR-1; the dashboard's
// "last-refreshed" column reads this field).
type catalogModelView struct {
	Provider         string  `json:"provider"`
	ModelID          string  `json:"model_id"`
	DisplayName      string  `json:"display_name"`
	Tier             int     `json:"tier"`
	ContextWindow    int     `json:"context_window"`
	MaxOutput        int     `json:"max_output"`
	SupportsImages   bool    `json:"supports_images"`
	SupportsTools    bool    `json:"supports_tools"`
	SupportsThinking bool    `json:"supports_thinking"`
	Source           string  `json:"source"`
	InputPrice       float64 `json:"input_price"`
	OutputPrice      float64 `json:"output_price"`
	CacheReadPrice   float64 `json:"cache_read_price"`
	CacheWritePrice  float64 `json:"cache_write_price"`
	ThinkingPrice    float64 `json:"thinking_price"`
	PricingSource    string  `json:"pricing_source"`
	UpdatedAt        string  `json:"updated_at,omitempty"`    // RFC3339; empty when unknown
	DiscoveredAt     string  `json:"discovered_at,omitempty"` // RFC3339; empty when unknown
}

func modelToCatalogView(m catalog.Model) catalogModelView {
	v := catalogModelView{
		Provider:         m.Provider,
		ModelID:          m.ModelID,
		DisplayName:      m.DisplayName,
		Tier:             int(m.Tier),
		ContextWindow:    m.ContextWindow,
		MaxOutput:        m.MaxOutput,
		SupportsImages:   m.Caps.SupportsImages,
		SupportsTools:    m.Caps.SupportsTools,
		SupportsThinking: m.Caps.SupportsThinking,
		Source:           m.Source,
		InputPrice:       m.Pricing.Input,
		OutputPrice:      m.Pricing.Output,
		CacheReadPrice:   m.Pricing.CacheRead,
		CacheWritePrice:  m.Pricing.CacheWrite,
		ThinkingPrice:    m.Pricing.Thinking,
		PricingSource:    m.Pricing.Source,
	}
	if !m.UpdatedAt.IsZero() {
		v.UpdatedAt = m.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if !m.DiscoveredAt.IsZero() {
		v.DiscoveredAt = m.DiscoveredAt.UTC().Format(time.RFC3339)
	}
	return v
}

func (s *Server) handleGetCatalogModels(w http.ResponseWriter, r *http.Request) {
	// Unknown-provider contract (M2.10 review MINOR-4): an unknown
	// provider returns an empty list with 200 OK, not 404. This is
	// the REST list-filter convention — "filtered to a set with no
	// matches" is a valid query result, not a missing resource. The
	// route is /api/catalog/models[/{provider}]; both shapes return
	// a JSON array (possibly empty). Cross-ref: ADR-1 §JSON shape
	// contracts pins the empty-array-not-404 rule for all list endpoints.
	if s.deps.Catalog == nil {
		writeJSON(w, http.StatusOK, []catalogModelView{})
		return
	}
	provider := r.PathValue("provider") // "" when matched at /api/catalog/models
	models := s.deps.Catalog.ListProvider(provider)
	out := make([]catalogModelView, 0, len(models))
	for _, m := range models {
		out = append(out, modelToCatalogView(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// putCatalogPricingRequest is the request body for PUT
// /api/catalog/pricing. **All five price fields are required.** PUT is
// strictly replace-not-patch — see the handler doc for the rationale.
// `*float64` distinguishes "field omitted" (nil) from "field set to 0"
// (which is a valid value for free models / inapplicable dimensions).
type putCatalogPricingRequest struct {
	InputPrice      *float64 `json:"input_price"`
	OutputPrice     *float64 `json:"output_price"`
	CacheReadPrice  *float64 `json:"cache_read_price"`
	CacheWritePrice *float64 `json:"cache_write_price"`
	ThinkingPrice   *float64 `json:"thinking_price"`
}

// handlePutCatalogPricing — PUT a user pricing override.
//
// **Contract: PUT is replace-not-patch.** The request body must
// include all five price fields; omitted fields are rejected with 400.
// This prevents the silent-zeroing footgun that would happen if a
// caller sent `{"input_price": 5}` expecting "update only input_price"
// — under replace semantics that request would zero output_price,
// cache_read_price, etc. Callers wanting partial updates must
// read-modify-write: GET the current pricing, mutate, PUT back.
//
// **Model row auto-creation:** if (provider, model_id) doesn't yet
// exist in catalog_models, the handler creates a placeholder model
// row at `source='seed'`. This satisfies the catalog_pricing FK
// without locking out future discovery — seed loses to discovery,
// openrouter, AND user on the precedence ladder (ADR-2 §Conflict
// resolution), so a subsequent discovery cycle can populate the
// model's display_name, tier, capabilities, etc. Writing
// `source='user'` for the model row (the obvious choice) would block
// discovery from ever touching it — see M2.10-review MAJOR-1.
func (s *Server) handlePutCatalogPricing(w http.ResponseWriter, r *http.Request) {
	if s.deps.CatalogStore == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog store unavailable")
		return
	}
	providerID := r.PathValue("provider")
	modelID := r.PathValue("model_id")
	if providerID == "" || modelID == "" {
		writeError(w, http.StatusBadRequest, "provider and model_id are required")
		return
	}

	var req putCatalogPricingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Strict-replace validation: every field must be present. Surface
	// the missing field name so the caller can fix their payload.
	missing := []string{}
	if req.InputPrice == nil {
		missing = append(missing, "input_price")
	}
	if req.OutputPrice == nil {
		missing = append(missing, "output_price")
	}
	if req.CacheReadPrice == nil {
		missing = append(missing, "cache_read_price")
	}
	if req.CacheWritePrice == nil {
		missing = append(missing, "cache_write_price")
	}
	if req.ThinkingPrice == nil {
		missing = append(missing, "thinking_price")
	}
	if len(missing) > 0 {
		writeError(w, http.StatusBadRequest,
			"PUT requires all five price fields (replace semantics); missing: "+strings.Join(missing, ", "))
		return
	}

	// Value validation (spec §6 / decision-log RM5 — surfaced by Gate
	// 3 cumulative review as MAJOR-1; the contract was promised but
	// not implemented). Reject:
	//   - NaN / +-Inf (would propagate through EstimateCost and
	//     corrupt usage_log cost rows);
	//   - negative prices (would invert the cheap-strategy sort);
	//   - prices above 10000 USD per 1M tokens (~3 orders of
	//     magnitude above the most expensive frontier model;
	//     anything higher is a fat-finger error).
	const maxPricePer1M = 10000.0
	type priceField struct {
		name string
		v    float64
	}
	fields := []priceField{
		{"input_price", *req.InputPrice},
		{"output_price", *req.OutputPrice},
		{"cache_read_price", *req.CacheReadPrice},
		{"cache_write_price", *req.CacheWritePrice},
		{"thinking_price", *req.ThinkingPrice},
	}
	for _, f := range fields {
		if math.IsNaN(f.v) {
			writeError(w, http.StatusBadRequest, f.name+" must be a real number (got NaN)")
			return
		}
		if math.IsInf(f.v, 0) {
			writeError(w, http.StatusBadRequest, f.name+" must be finite (got Inf)")
			return
		}
		if f.v < 0 {
			writeError(w, http.StatusBadRequest, f.name+" must be non-negative")
			return
		}
		if f.v > maxPricePer1M {
			writeError(w, http.StatusBadRequest,
				f.name+" exceeds sanity cap of $10000 per 1M tokens")
			return
		}
	}

	// Auto-create model row at source='seed' so the FK is satisfied
	// AND a future discovery refresh can still populate the row's
	// metadata. See handler doc above + M2.10-review MAJOR-1.
	if err := s.deps.CatalogStore.UpsertModel(r.Context(), catalog.Model{
		Provider: providerID,
		ModelID:  modelID,
		Source:   catalog.SourceSeed,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "upsert model: "+err.Error())
		return
	}

	if err := s.deps.CatalogStore.UpsertPricing(r.Context(), providerID, modelID, catalog.Pricing{
		Input:      *req.InputPrice,
		Output:     *req.OutputPrice,
		CacheRead:  *req.CacheReadPrice,
		CacheWrite: *req.CacheWritePrice,
		Thinking:   *req.ThinkingPrice,
		Source:     catalog.SourceUser,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "upsert pricing: "+err.Error())
		return
	}

	// Invalidate the Registry cache so subsequent reads (cost
	// computation, dashboard /api/models) see the new values.
	if s.deps.Catalog != nil {
		s.deps.Catalog.Invalidate()
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteCatalogPricing(w http.ResponseWriter, r *http.Request) {
	if s.deps.CatalogStore == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog store unavailable")
		return
	}
	providerID := r.PathValue("provider")
	modelID := r.PathValue("model_id")
	if providerID == "" || modelID == "" {
		writeError(w, http.StatusBadRequest, "provider and model_id are required")
		return
	}

	// Idempotent — DeletePricing returns nil error when no row
	// exists, so 200 OK is the right response for missing-row too.
	if err := s.deps.CatalogStore.DeletePricing(r.Context(), providerID, modelID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete pricing: "+err.Error())
		return
	}

	if s.deps.Catalog != nil {
		s.deps.Catalog.Invalidate()
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// catalogProviderView is the JSON shape for /api/catalog/providers.
// One row per provider in catalog_provider_meta. Distinct from
// /api/providers which dumps the static KnownProviders map (no
// runtime state).
type catalogProviderView struct {
	Provider                 string `json:"provider"`
	DiscoveryEnabled         bool   `json:"discovery_enabled"`
	SubscriptionDiscoverable bool   `json:"subscription_discoverable"`
	LastDiscoveredAt         string `json:"last_discovered_at,omitempty"`
	LastDiscoveryError       string `json:"last_discovery_error,omitempty"`
	BackoffStep              int    `json:"backoff_step"`
	NextDiscoveryAfter       string `json:"next_discovery_after,omitempty"`
}

func (s *Server) handleGetCatalogProviders(w http.ResponseWriter, r *http.Request) {
	if s.deps.CatalogStore == nil {
		writeJSON(w, http.StatusOK, []catalogProviderView{})
		return
	}
	metas, err := s.deps.CatalogStore.ListProviderMetas(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list provider metas: "+err.Error())
		return
	}
	out := make([]catalogProviderView, 0, len(metas))
	for _, m := range metas {
		v := catalogProviderView{
			Provider:                 m.Provider,
			DiscoveryEnabled:         m.DiscoveryEnabled,
			SubscriptionDiscoverable: m.SubscriptionDiscoverable,
			LastDiscoveryError:       m.LastDiscoveryError,
			BackoffStep:              m.BackoffStep,
		}
		if !m.LastDiscoveredAt.IsZero() {
			v.LastDiscoveredAt = m.LastDiscoveredAt.UTC().Format(time.RFC3339)
		}
		if !m.NextDiscoveryAfter.IsZero() {
			v.NextDiscoveryAfter = m.NextDiscoveryAfter.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Recompute Route (Models Discovery M3.1) ──
//
// POST /api/catalog/recompute re-applies *current* catalog pricing to
// historical apikey usage_log rows in a date range. Subscription rows
// are skipped — their cost is 0 in usage_log and `subscription_savings`
// is computed at query time in the /api/usage/summary endpoint
// (via `catalog.Registry.EstimateCost` after M1's rewires), so live
// pricing already flows through automatically.
//
// Contract (ADR-3 §Part F):
//   - apikey rows whose stored cost differs from EstimateCost are
//     updated to the fresh value via Store.UpdateUsageCost
//   - cost_source is never touched
//   - dry_run=true returns the deltas in the response shape without
//     writing
//   - JWT required (operator action, mutates user-visible data)

type recomputeReq struct {
	From   string `json:"from"`    // RFC3339 or YYYY-MM-DD
	To     string `json:"to"`      // RFC3339 or YYYY-MM-DD
	DryRun bool   `json:"dry_run"` // preview without persisting
}

// recomputeResp is the JSON response from POST /api/catalog/recompute.
//
// TotalCostDelta semantics under concurrency: this value is advisory
// when concurrent recompute calls overlap the same time range. Both
// callers compute newCost from the same Catalog snapshot, so the final
// row.cost is identical regardless of order (benign last-write-wins),
// but each response double-counts its own delta. Operators inspecting
// the delta after concurrent overlap should treat it as "this caller's
// contribution to convergence", not a global tally. The serialised
// post-recompute state is authoritative; query usage_log for the
// definitive cost view.
type recomputeResp struct {
	RowsInspected           int     `json:"rows_inspected"`
	RowsUpdated             int     `json:"rows_updated"`
	RowsSkippedUnknownModel int     `json:"rows_skipped_unknown_model"`
	TotalCostDelta          float64 `json:"total_cost_delta_usd"`
	SubscriptionNote        string  `json:"subscription_note"`
}

// parseRecomputeTime accepts RFC3339 (the dashboard's likely format)
// or YYYY-MM-DD (operator-friendly curl). The `isUpperBound` flag
// shifts a date-only input to end-of-day (23:59:59.999999999 UTC) so
// `to: "2026-05-12"` means "through the end of 2026-05-12" instead of
// "midnight at the START of 2026-05-12" — the latter would exclude
// every row on that day, the opposite of operator intent (M3.1 review
// MINOR-2).
func parseRecomputeTime(s string, isUpperBound bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if isUpperBound {
			// End of the named day (UTC). The range query uses
			// `< to`, so anchoring at 23:59:59.999... includes
			// every wall-clock row on the date.
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want RFC3339 or YYYY-MM-DD)", s)
}

// recomputeMaxRangeDays caps the date span a single recompute request
// may scan. ListUsageInRange issues one bounded SELECT so the SQL side
// is not a runaway, but the in-memory []UsageEntry returned to the
// handler is unbounded for an open-ended range. 365 days is well past
// any operational use case while keeping the worst-case allocation
// bounded by typical retention windows.
const recomputeMaxRangeDays = 365

// recomputeCostEpsilon defines the threshold below which a recompute
// is treated as a no-op short-circuit. Floating-point equality on
// EstimateCost output would treat ULP-different reconvergences (e.g.,
// a rounding drift after a price-table edit) as updates, inflating
// rows_updated and total_cost_delta_usd by ~1e-15 each. 1e-9 USD
// (= 1 picoUSD) is many orders of magnitude below any plausible
// billable cost and matches the epsilon already used by the M3
// integration tests for pricing assertions.
const recomputeCostEpsilon = 1e-9

func (s *Server) handleRecompute(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil || s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "recompute requires catalog + store")
		return
	}

	var req recomputeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	from, err := parseRecomputeTime(req.From, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "from: "+err.Error())
		return
	}
	to, err := parseRecomputeTime(req.To, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "to: "+err.Error())
		return
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if to.Sub(from) > recomputeMaxRangeDays*24*time.Hour {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("recompute date range cannot exceed %d days", recomputeMaxRangeDays))
		return
	}

	// Only apikey rows are recomputed (ADR-3 §Part F). Subscription
	// cost is structurally 0 and savings are query-time computed.
	rows, err := s.deps.Store.ListUsageInRange(r.Context(), from, to, "apikey")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list usage: "+err.Error())
		return
	}

	const subscriptionNote = "subscription_savings auto-reflects current pricing at next /api/usage/summary read"
	var updated int
	var skippedUnknownModel int
	var delta float64
	for _, row := range rows {
		// Thinking tokens aren't stored separately in usage_log
		// today — they're folded into Output. ADR-3 §Part F is
		// explicit about this; the recompute matches the original
		// EstimateCost call's token-breakdown shape from the record
		// write to keep cost arithmetic consistent.
		breakdown := catalog.TokenBreakdown{
			Input:        row.InputTokens,
			Output:       row.OutputTokens,
			CacheReadIn:  row.CacheReadTokens,
			CacheWriteIn: row.CacheWriteTokens,
			Thinking:     0,
		}
		newCost := s.deps.Catalog.EstimateCost(row.Provider, row.Model, breakdown)
		// Preserve historical cost when the catalog no longer knows the
		// model. EstimateCost returns 0 for purged (provider, model)
		// pairs (e.g., a model removed by discovery); blindly writing
		// that 0 over a real historical cost is silent data loss. Surface
		// the count so operators can investigate the catalog drift.
		if newCost == 0 && row.Cost > 0 {
			skippedUnknownModel++
			continue
		}
		if math.Abs(newCost-row.Cost) < recomputeCostEpsilon {
			continue
		}
		if !req.DryRun {
			if err := s.deps.Store.UpdateUsageCost(r.Context(), row.ID, newCost); err != nil {
				slog.Warn("recompute: update failed",
					"id", row.ID, "provider", row.Provider, "model", row.Model, "err", err)
				continue
			}
		}
		updated++
		delta += newCost - row.Cost
	}

	writeJSON(w, http.StatusOK, recomputeResp{
		RowsInspected:           len(rows),
		RowsUpdated:             updated,
		RowsSkippedUnknownModel: skippedUnknownModel,
		TotalCostDelta:          delta,
		SubscriptionNote:        subscriptionNote,
	})
}

// ── Status Route ──

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	conns, _ := s.deps.Store.ListConnections(store.ConnectionFilter{})
	active, cooldown, errored := 0, 0, 0
	for _, c := range conns {
		switch c.State {
		case "active", "idle":
			active++
		case "cooldown", "rate_limited":
			cooldown++
		case "errored", "disabled":
			errored++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"active":   active,
		"cooldown": cooldown,
		"errored":  errored,
		"total":    len(conns),
	})
}

// ── Credential Detection Routes ──

func (s *Server) handleDetectClaude(w http.ResponseWriter, r *http.Request) {
	result, _ := detect.DetectClaude()
	writeJSON(w, http.StatusOK, result)
}

// handleDetectCodex is the OpenAI/Codex CLI sibling of handleDetectClaude
// (initiative 20260513-openai-autodetect). Probes the local filesystem
// for `~/.codex/auth.json` and returns a safe `{found, subscription_type,
// expired}` shape — never exposes raw tokens. The dashboard's
// `detectCodex()` API wrapper calls this at Add-Provider-modal mount
// time to decide whether to render the "Codex CLI detected" auto-detect
// card.
func (s *Server) handleDetectCodex(w http.ResponseWriter, r *http.Request) {
	result, _ := detect.DetectCodex()
	writeJSON(w, http.StatusOK, result)
}

// ── OpenAI OAuth Routes (legacy device-code, retired in M2.8) ──
//
// These handlers used to drive a device-code login. M2.8 retired the
// flow in favor of PKCE (see routes_auth_oauth.go). The handlers below
// return 410 Gone with a pointer at the replacement so any clients still
// polling the old endpoints fail loudly. M3.7 deletes them plus the
// OpenAIAuth dependency.

func (s *Server) handleOpenAIDeviceStart(w http.ResponseWriter, r *http.Request) {
	s.handleOpenAIDeviceStartDeprecated(w, r)
}

func (s *Server) handleOpenAIDevicePoll(w http.ResponseWriter, r *http.Request) {
	s.handleOpenAIDevicePollDeprecated(w, r)
}

// ── Routing Analytics Routes ──

func (s *Server) handleGetRoutingSummary(w http.ResponseWriter, r *http.Request) {
	filter := store.UsageFilter{
		Provider: r.URL.Query().Get("provider"),
	}
	summary, err := s.deps.Store.RoutingSummary(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Enrich with live session data
	var affinityEntries, bridgeActive int
	if s.deps.SmartRouter != nil {
		affinityEntries = s.deps.SmartRouter.Affinity.Len()
	}
	if s.deps.ConversationStore != nil {
		// Count active bridges from affinity cache
		// (approximation — we count conversations as proxy)
		bridgeActive = s.deps.ConversationStore.Len()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"summary":              summary,
		"active_sessions":      affinityEntries,
		"active_conversations": bridgeActive,
		"memory_usage_bytes": func() int64 {
			if s.deps.ConversationStore != nil {
				return s.deps.ConversationStore.Size()
			}
			return 0
		}(),
	})
}

func (s *Server) handleGetRoutingLog(w http.ResponseWriter, r *http.Request) {
	// Query routing_log with same filters as usage
	filter := store.UsageFilter{
		Provider: r.URL.Query().Get("provider"),
		Limit:    50,
	}

	// We reuse the routing_log query
	entries, err := s.deps.Store.QueryRoutingLog(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
