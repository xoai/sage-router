package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/imports"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/auth/providers"
	"sage-router/internal/auth/refresh"
	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// ── Subscription Auth Routes (M2.8) ──
//
// These handlers + the OAuthBridge dependency together implement the
// dashboard's subscription-auth UX. The legacy device-code endpoints at
// /api/auth/openai/device/* return 410 Gone for one release per the M2.8
// deprecation strategy; final removal happens in M3.7.

type startFlowRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

type startFlowResponse struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

// handleOAuthStart begins a PKCE flow on behalf of the dashboard.
// Captures the request Origin so the bridge can redirect back after the
// callback. Returns the authorize URL the dashboard opens in a new tab.
//
// TOS gate: returns 412 with {requires_tos: true, message} if the user
// hasn't acknowledged the subscription-auth TOS.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	s.startFlowImpl(w, r, true /* dashboard */)
}

// handleOAuthCLIHandoff is the same as start, but marks the flow as
// CLI-initiated (no DashboardSID / Origin). The CLI typically uses this
// when sage-router is already running.
func (s *Server) handleOAuthCLIHandoff(w http.ResponseWriter, r *http.Request) {
	s.startFlowImpl(w, r, false /* CLI */)
}

func (s *Server) startFlowImpl(w http.ResponseWriter, r *http.Request, fromDashboard bool) {
	if s.deps.OAuthBridge == nil {
		writeError(w, http.StatusServiceUnavailable, "subscription auth not configured")
		return
	}

	// TOS gate (AC33, AC40).
	if !auth.IsTOSAcknowledged(s.deps.Store) {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"requires_tos": true,
			"message":      auth.TOSText,
		})
		return
	}

	var req startFlowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	canon, err := providers.Resolve(req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown provider: "+req.Provider)
		return
	}

	// Reject if this provider's bridge port is unavailable — gives the
	// dashboard a clear "port_busy" response rather than a stuck flow
	// the user will eventually time out on.
	if health := s.deps.OAuthBridge.Health(canon); health != oauth.HealthAvailable {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":    "bridge port unavailable",
			"provider": canon,
			"health":   health,
			"hint":     "stop any running CLI tool that uses port 1455 (openai) or 53692 (anthropic), then click retry",
		})
		return
	}

	flow, err := oauth.NewFlow(canon, req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot start flow: "+err.Error())
		return
	}
	flow.Priority = req.Priority
	if fromDashboard {
		flow.Origin = r.Header.Get("Origin")
	}

	s.deps.OAuthBridge.Register(flow)

	authURL := flow.AuthorizeURL(s.deps.OAuthBridge.CallbackURL(canon))
	writeJSON(w, http.StatusOK, startFlowResponse{
		AuthorizeURL: authURL,
		State:        flow.State,
	})
}

// handleOAuthStatus is polled by the dashboard / CLI to detect callback
// completion. Returns the current status as recorded by the bridge.
func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.OAuthBridge == nil {
		writeError(w, http.StatusServiceUnavailable, "subscription auth not configured")
		return
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		writeError(w, http.StatusBadRequest, "state parameter required")
		return
	}
	info := s.deps.OAuthBridge.Status(state)
	out := map[string]any{"status": string(info.Status)}
	if info.ConnectionID != "" {
		out["conn_id"] = info.ConnectionID
	}
	if info.Error != "" {
		out["error"] = info.Error
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOAuthHealth returns the bridge bind-status for each PKCE provider.
// The dashboard polls this every few seconds while the connection-add
// modal is open so it can grey out "Login with subscription" buttons for
// ports that are currently busy (AC44c).
func (s *Server) handleOAuthHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.OAuthBridge == nil {
		writeJSON(w, http.StatusOK, map[string]string{
			"openai":    "stopped",
			"anthropic": "stopped",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"openai":    s.deps.OAuthBridge.Health("openai"),
		"anthropic": s.deps.OAuthBridge.Health("anthropic"),
	})
}

// handleOAuthRebind tries to re-bind a provider's bridge port (AC44d).
// Called when the user clicks "retry" after stopping a conflicting CLI tool.
func (s *Server) handleOAuthRebind(w http.ResponseWriter, r *http.Request) {
	if s.deps.OAuthBridge == nil {
		writeError(w, http.StatusServiceUnavailable, "subscription auth not configured")
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	canon, err := providers.Resolve(req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown provider: "+req.Provider)
		return
	}
	health := s.deps.OAuthBridge.TryRebind(canon)
	writeJSON(w, http.StatusOK, map[string]string{"health": health, "provider": canon})
}

// ── Import ──

type importRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	Path     string `json:"path,omitempty"` // optional override
}

// handleImport reads a provider's CLI credential file and creates a
// subscription Connection.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	// TOS gate identical to OAuth flows.
	if !auth.IsTOSAcknowledged(s.deps.Store) {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"requires_tos": true,
			"message":      auth.TOSText,
		})
		return
	}

	var req importRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	canon, err := providers.Resolve(req.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown provider: "+req.Provider)
		return
	}

	cred, err := imports.ImportFromCLI(canon, req.Path)
	if err != nil {
		// AC18: macOS Keychain hint when Claude's credential file is
		// missing on darwin.
		if canon == "anthropic" && imports.MacOSClaudeKeychainHint(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": err.Error(),
				"hint":  imports.KeychainHintMessage,
			})
			return
		}
		if errors.Is(err, imports.ErrFileNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "import failed: "+err.Error())
		return
	}

	// Copilot imports come back with an empty AccessToken — the GH OAuth
	// token in cred.RefreshToken is the long-lived material; we need to
	// run the 2-step refresh to mint a usable Copilot bearer.
	if canon == "github-copilot" && cred.AccessToken == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		fresh, refreshErr := refresh.Refresh(ctx, cred)
		if refreshErr != nil {
			writeError(w, http.StatusBadGateway,
				"imported copilot credential but initial refresh failed: "+refreshErr.Error())
			return
		}
		cred = fresh
		cred.Provider = canon
	}

	connID, err := s.createSubscriptionConnection(canon, req.Name, req.Priority, cred)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save connection: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       connID,
		"provider": canon,
		"source":   "import",
	})
}

// ── TOS acceptance ──

func (s *Server) handleTOSAccept(w http.ResponseWriter, r *http.Request) {
	if err := auth.AcknowledgeTOS(s.deps.Store); err != nil {
		writeError(w, http.StatusInternalServerError, "save tos ack: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Bridge handler implementation ──

// oauthBridgeHandler implements oauth.BridgeHandler — the bridge invokes
// this after a successful token exchange. We create the Connection row,
// register with the selector, and tell the bridge where to redirect the
// user's browser back to.
type oauthBridgeHandler struct {
	srv *Server
}

func (h *oauthBridgeHandler) OnFlowComplete(ctx context.Context, flow *oauth.Flow, resp *oauth.TokenResponse) (connID, redirectTo string, err error) {
	cred := flow.ToCredential(resp)
	connID, err = h.srv.createSubscriptionConnection(flow.Provider, flow.ConnName, flow.Priority, cred)
	if err != nil {
		return "", "", err
	}
	if flow.Origin != "" {
		// Send the browser back to the dashboard's oauth-complete page
		// (M3.5 will render the connection in the list).
		redirectTo = flow.Origin + "/dashboard/oauth-complete?status=ok&conn_id=" + connID
	}
	return connID, redirectTo, nil
}

// createSubscriptionConnection persists a Connection row built from a
// fresh Credential and registers it with the in-memory selector so the
// next request can use it without a server restart. Shared by the OAuth
// flow path and the import path.
func (s *Server) createSubscriptionConnection(providerID, name string, priority int, cred *auth.Credential) (string, error) {
	if name == "" {
		name = providerID
	}
	id := generateRequestID()

	conn := &store.Connection{
		ID:           id,
		Provider:     providerID,
		Name:         name,
		AuthType:     auth.AuthTypeSubscription,
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		Priority:     priority,
		State:        string(provider.StateIdle),
	}
	if !cred.ExpiresAt.IsZero() {
		t := cred.ExpiresAt
		conn.ExpiresAt = &t
	}
	if cred.AccountID != "" || len(cred.ExtraData) > 0 {
		// Match the nested shape AuthStore.GetCredential decodes into so
		// ExtraData survives round-trips through the store. Without
		// nesting, every refresh / dashboard load silently drops the
		// imported extras (subscription_type, github_user, scope).
		pd := struct {
			AccountID string         `json:"account_id,omitempty"`
			Extra     map[string]any `json:"extra,omitempty"`
		}{
			AccountID: cred.AccountID,
			Extra:     cred.ExtraData,
		}
		raw, _ := json.Marshal(pd)
		conn.ProviderData = raw
	}
	if err := s.deps.Store.CreateConnection(conn); err != nil {
		return "", fmt.Errorf("create connection: %w", err)
	}

	provConn := provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType)
	s.deps.ProviderSelector.Register(provConn)
	return id, nil
}

// ── Legacy device-code deprecation (M2.8 → M3.7 deletion) ──

const deviceFlowGoneMessage = "OpenAI device-code flow has been retired. " +
	"Use POST /api/auth/oauth/start (dashboard) or " +
	"`sage-router auth login --provider openai` (CLI) to perform PKCE login instead."

func (s *Server) handleOpenAIDeviceStartDeprecated(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]any{
		"error":   "device-flow retired",
		"message": deviceFlowGoneMessage,
		"replacement": map[string]string{
			"dashboard": "POST /api/auth/oauth/start",
			"cli":       "sage-router auth login --provider openai",
		},
	})
}

func (s *Server) handleOpenAIDevicePollDeprecated(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]any{
		"error":   "device-flow retired",
		"message": deviceFlowGoneMessage,
	})
}
