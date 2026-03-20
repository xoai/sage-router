package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/detect"
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

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	conns, err := s.deps.Store.ListConnections(store.ConnectionFilter{Provider: provider})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Redact sensitive fields
	type safeConn struct {
		ID        string    `json:"id"`
		Provider  string    `json:"provider"`
		Name      string    `json:"name"`
		AuthType  string    `json:"auth_type"`
		Priority  int       `json:"priority"`
		State     string    `json:"state"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	var safe []safeConn
	for _, c := range conns {
		safe = append(safe, safeConn{
			ID:        c.ID,
			Provider:  c.Provider,
			Name:      c.Name,
			AuthType:  c.AuthType,
			Priority:  c.Priority,
			State:     c.State,
			CreatedAt: c.CreatedAt,
			UpdatedAt: c.UpdatedAt,
		})
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

	// For auto_detect connections, read credentials from disk now
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
		conn.AuthType = "oauth" // store as oauth since we now have a real token
	}

	if err := s.deps.Store.CreateConnection(&conn); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Register with selector
	provConn := provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType)
	s.deps.ProviderSelector.Register(provConn)

	writeJSON(w, http.StatusCreated, map[string]any{"id": conn.ID})
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

	// Simple test: try a models list or a simple completion
	exec, ok := s.deps.Executors[conn.Provider]
	if !ok {
		exec = s.deps.Executors["default"]
	}
	if exec == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no executor"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": conn.Provider})
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

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.deps.Store.ListAPIKeys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
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

	key := &store.APIKey{
		ID:        generateRequestID(),
		Name:      req.Name,
		KeyHash:   keyHash,
		Prefix:    prefix,
		CreatedAt: time.Now(),
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

func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	filter := store.UsageFilter{
		Provider: r.URL.Query().Get("provider"),
		Model:    r.URL.Query().Get("model"),
		Limit:    100,
	}

	entries, err := s.deps.Store.QueryUsage(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleGetUsageSummary(w http.ResponseWriter, r *http.Request) {
	filter := store.UsageFilter{
		Provider: r.URL.Query().Get("provider"),
	}
	summary, err := s.deps.Store.UsageSummary(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// ── Provider & Model Catalog Routes ──

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, config.KnownProviders)
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

	// Filter catalog to models from active providers, using provider/model format
	type modelEntry struct {
		ID          string  `json:"id"`
		Provider    string  `json:"provider"`
		DisplayName string  `json:"display_name"`
		InputPrice  float64 `json:"input_price,omitempty"`
		OutputPrice float64 `json:"output_price,omitempty"`
	}
	var models []modelEntry
	for _, m := range config.ModelCatalog {
		if activeProviders[m.Provider] {
			models = append(models, modelEntry{
				ID:          m.Provider + "/" + m.ID,
				Provider:    m.Provider,
				DisplayName: m.DisplayName,
				InputPrice:  m.InputPrice,
				OutputPrice: m.OutputPrice,
			})
		}
	}
	writeJSON(w, http.StatusOK, models)
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

// ── OpenAI OAuth Routes ──

func (s *Server) handleOpenAIDeviceStart(w http.ResponseWriter, r *http.Request) {
	if s.deps.OpenAIAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "OpenAI OAuth not configured")
		return
	}

	resp, err := s.deps.OpenAIAuth.StartDeviceFlow()
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to start device flow: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleOpenAIDevicePoll(w http.ResponseWriter, r *http.Request) {
	if s.deps.OpenAIAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "OpenAI OAuth not configured")
		return
	}

	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserCode == "" {
		writeError(w, http.StatusBadRequest, "user_code required")
		return
	}

	tokenResp, err := s.deps.OpenAIAuth.PollForToken(req.UserCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Create a connection with the OAuth token
	conn := store.Connection{
		ID:          generateRequestID(),
		Provider:    "openai",
		Name:        "OpenAI (OAuth)",
		AuthType:    "oauth",
		AccessToken: tokenResp.AccessToken,
		State:       "idle",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if tokenResp.RefreshToken != "" {
		conn.RefreshToken = tokenResp.RefreshToken
	}

	if err := s.deps.Store.CreateConnection(&conn); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	provConn := provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType)
	s.deps.ProviderSelector.Register(provConn)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"connection_id": conn.ID,
	})
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
		"summary":           summary,
		"active_sessions":   affinityEntries,
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
