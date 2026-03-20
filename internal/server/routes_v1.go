package server

import (
	"bufio"
	"context"
	crypto_rand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"sage-router/internal/auth/detect"
	"sage-router/internal/bypass"
	"sage-router/internal/config"
	"sage-router/internal/cost"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/routing"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	"sage-router/internal/usage"
	"sage-router/pkg/canonical"
	"sage-router/pkg/sse"
)

// handleChatCompletions is the core routing handler for /v1/chat/completions, /v1/messages, etc.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	requestID := generateRequestID()

	// Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	// Detect source format
	sourceFormat := translate.DetectSourceFormat(r.URL.Path, body)

	// Block proxy requests until initial setup is complete (password created)
	if s.deps.Auth.NeedsSetup() {
		writeError(w, http.StatusServiceUnavailable, "sage-router setup required — open the dashboard to create a password")
		return
	}

	// Validate API key
	// If any keys have ever been created (password is set = setup complete),
	// require a valid key. This prevents deleted keys from silently working
	// when the last key is removed.
	var authenticatedKey *store.APIKey
	hasKeys, _ := s.deps.Store.HasAPIKeys()
	apiKey := extractAPIKey(r)

	if hasKeys {
		// Keys exist — validate the provided key
		if apiKey == "" {
			writeError(w, http.StatusUnauthorized, "API key required")
			return
		}
		keyHash := s.deps.Auth.HashAPIKey(apiKey)
		key, err := s.deps.Store.GetAPIKeyByHash(keyHash)
		if err != nil || key == nil {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		authenticatedKey = key

		// Rate limit check (§34)
		if key.RateLimitRPM > 0 && s.deps.RateLimiter != nil {
			if !s.deps.RateLimiter.Allow(key.ID, key.RateLimitRPM) {
				writeError(w, http.StatusTooManyRequests, fmt.Sprintf("rate limit exceeded: %d requests/min for key %q", key.RateLimitRPM, key.Name))
				return
			}
		}

		// Budget check (§34)
		if key.BudgetMonthly > 0 {
			spent, err := s.deps.Store.GetMonthlySpend(key.ID)
			if err == nil && key.BudgetHardLimit && spent >= key.BudgetMonthly {
				writeError(w, http.StatusPaymentRequired, fmt.Sprintf("monthly budget exceeded for key %q: $%.2f / $%.2f", key.Name, spent, key.BudgetMonthly))
				return
			}
		}
	} else if apiKey != "" {
		// No keys in DB but a key was provided — it's invalid (possibly deleted)
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}

	// Parse model from request
	model, stream := extractModelAndStream(body)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}

	// Pre-pipeline bypass check (ADR §26)
	if s.deps.BypassFilter != nil {
		bypassReq := extractBypassReq(body, model)
		if result := s.deps.BypassFilter.Check(bypassReq); result != nil {
			slog.Info("bypass", "pattern", result.PatternName, "model", model)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Sage-Bypass", result.PatternName)
			w.Write(result.Response)
			return
		}
	}

	// Per-key routing strategy override (§34)
	if authenticatedKey != nil && authenticatedKey.RoutingStrategy != "" && model == "auto" {
		model = "auto:" + authenticatedKey.RoutingStrategy
	}

	// Resolve provider and model
	providerID, resolvedModel, isCombo, comboModels := s.resolveModel(model, body)

	// ACL check — enforce allowed models (§34)
	if authenticatedKey != nil && authenticatedKey.AllowedModels != "*" {
		target := resolvedModel
		if providerID != "" {
			target = providerID + "/" + resolvedModel
		}
		if isCombo {
			// Filter combo list to only ACL-permitted models.
			// This prevents bypassing ACL by placing a disallowed model
			// before an allowed one in the combo sequence.
			var filtered []string
			for _, m := range comboModels {
				if matchModelPattern(m, authenticatedKey.AllowedModels) {
					filtered = append(filtered, m)
				}
			}
			if len(filtered) == 0 {
				writeError(w, http.StatusForbidden, fmt.Sprintf("no permitted models in combo for key %q: allowed=%s", authenticatedKey.Name, authenticatedKey.AllowedModels))
				return
			}
			comboModels = filtered
		} else if !matchModelPattern(target, authenticatedKey.AllowedModels) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("model %q not allowed for key %q: allowed=%s", target, authenticatedKey.Name, authenticatedKey.AllowedModels))
			return
		}
	}

	if isCombo {
		var comboKeyID string
		if authenticatedKey != nil {
			comboKeyID = authenticatedKey.ID
		}
		s.handleComboRequest(w, r, body, sourceFormat, comboModels, stream, requestID, startTime, comboKeyID)
		return
	}

	// Select connection
	conn, retryAfter, err := s.selectConnection(providerID, resolvedModel, nil)
	if err != nil {
		if retryAfter > 0 {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		}
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("no available connection for %s: %v", providerID, err))
		return
	}

	// Derive key ID for usage tracking
	var apiKeyID string
	if authenticatedKey != nil {
		apiKeyID = authenticatedKey.ID
	}

	// Execute with fallback
	s.executeRequest(w, r, body, sourceFormat, providerID, resolvedModel, stream, conn, nil, requestID, startTime, apiKeyID)
}

// requestContext carries metadata through the request lifecycle for post-response hooks.
type requestContext struct {
	firstMsg    string // first user message (session key)
	requestBody []byte // raw request body (for conversation store)
	apiKeyID    string // authenticated API key ID (for usage tracking)
}

func (s *Server) executeRequest(
	w http.ResponseWriter, r *http.Request,
	body []byte,
	sourceFormat canonical.Format,
	providerID, model string,
	stream bool,
	conn *ConnectionInfo,
	excludeIDs []string,
	requestID string,
	startTime time.Time,
	apiKeyID string,
) {
	// Build request context for post-response hooks
	reqCtx := &requestContext{
		firstMsg:    extractFirstUserMsg(body),
		requestBody: body,
		apiKeyID:    apiKeyID,
	}
	s.executeRequestWithCtx(w, r, body, sourceFormat, providerID, model, stream, conn, excludeIDs, requestID, startTime, reqCtx)
}

func (s *Server) executeRequestWithCtx(
	w http.ResponseWriter, r *http.Request,
	body []byte,
	sourceFormat canonical.Format,
	providerID, model string,
	stream bool,
	conn *ConnectionInfo,
	excludeIDs []string,
	requestID string,
	startTime time.Time,
	reqCtx *requestContext,
) {
	// Determine target format
	targetFormat := translate.DetectTargetFormat(providerID)

	// Translate request: source → canonical → target
	canonReq, targetBody, err := s.deps.TranslateRegistry.TranslateRequest(sourceFormat, targetFormat, body, translate.TranslateOpts{
		Model:    model,
		Provider: providerID,
		Stream:   stream,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("translation error: %v", err))
		return
	}

	// Override model in translated request
	if canonReq != nil {
		canonReq.Model = model
	}

	// Stage ⑤b: Inject cache hints (cost optimization)
	if canonReq != nil && cost.InjectCacheHints(canonReq, providerID) {
		// Re-serialize with cache hints applied
		if tgt, ok := s.deps.TranslateRegistry.Get(targetFormat); ok {
			if rewritten, err := tgt.FromCanonical(canonReq, translate.TranslateOpts{
				Model: model, Provider: providerID, Stream: stream,
			}); err == nil {
				targetBody = rewritten
			}
		}
	}

	// Stage ④+: Context bridge injection on model switch (ADR §29)
	if canonReq != nil && s.deps.ConversationStore != nil && s.deps.SmartRouter != nil {
		firstMsg := extractFirstUserMsg(body)
		if firstMsg != "" {
			if entry := s.deps.SmartRouter.Affinity.Get(firstMsg); entry != nil {
				currentModel := providerID + "/" + model
				previousModel := entry.Provider + "/" + entry.Model
				if currentModel != previousModel && entry.TurnCount > 1 {
					// Affinity break — inject bridge
					history := s.deps.ConversationStore.GetHistory(firstMsg)
					if history != nil {
						bridge := routing.BuildContextBridge(history, routing.DefaultBridgeConfig().MaxTokens)
						if bridge != "" {
							canonReq.System = append([]canonical.SystemBlock{{Text: bridge}}, canonReq.System...)
							// Mark bridge as active with 3-turn lifecycle
						entry.BridgeActive = true
						entry.BridgeTurnsLeft = 3
						slog.Info("bridge injected", "from", previousModel, "to", currentModel, "tokens", len(bridge)/4)
							// Re-serialize
							if tgt, ok := s.deps.TranslateRegistry.Get(targetFormat); ok {
								if rewritten, err := tgt.FromCanonical(canonReq, translate.TranslateOpts{
									Model: model, Provider: providerID, Stream: stream,
								}); err == nil {
									targetBody = rewritten
								}
							}
						}
					}
				}
			}
		}
	}

	// Get executor
	exec, ok := s.deps.Executors[providerID]
	if !ok {
		exec = s.deps.Executors["default"]
	}
	if exec == nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("no executor for provider %s", providerID))
		return
	}

	// Execute upstream call
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	result, err := exec.Execute(ctx, &executor.ExecuteRequest{
		Model:       model,
		Body:        targetBody,
		Stream:      stream,
		Credentials: conn.Credentials,
		Endpoint:    conn.Endpoint,
	})
	if err != nil {
		slog.Error("upstream error", "provider", providerID, "error", err)
		s.markConnectionResult(conn.ID, model, 0, err)
		// Try fallback
		if excludeIDs == nil {
			excludeIDs = []string{}
		}
		excludeIDs = append(excludeIDs, conn.ID)
		nextConn, _, nextErr := s.selectConnection(providerID, model, excludeIDs)
		if nextErr == nil {
			slog.Info("falling back", "provider", providerID, "connection", nextConn.ID)
			s.executeRequestWithCtx(w, r, body, sourceFormat, providerID, model, stream, nextConn, excludeIDs, requestID, startTime, reqCtx)
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream error: %v", err))
		return
	}
	defer result.Body.Close()

	// Check for error status
	if result.StatusCode >= 400 {
		respBody, _ := io.ReadAll(result.Body)
		statusCode := result.StatusCode

		// Mark connection state based on error
		s.markConnectionResult(conn.ID, model, statusCode, nil)

		// Fallback on retryable errors
		if executor.IsFallbackEligible(statusCode) {
			if excludeIDs == nil {
				excludeIDs = []string{}
			}
			excludeIDs = append(excludeIDs, conn.ID)
			nextConn, _, nextErr := s.selectConnection(providerID, model, excludeIDs)
			if nextErr == nil {
				slog.Info("falling back on error",
					"provider", providerID,
					"status", statusCode,
					"connection", nextConn.ID,
				)
				s.executeRequest(w, r, body, sourceFormat, providerID, model, stream, nextConn, excludeIDs, requestID, startTime, reqCtx.apiKeyID)
				return
			}
		}

		// Forward error to client
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		w.Write(respBody)
		return
	}

	latencyTTFB := time.Since(startTime)

	if stream {
		s.streamResponse(w, r, result, sourceFormat, targetFormat, model, requestID, startTime, latencyTTFB, providerID, conn.ID, reqCtx)
	} else {
		s.forwardResponse(w, result, sourceFormat, targetFormat, model, requestID, startTime, latencyTTFB, providerID, conn.ID, reqCtx)
	}

	// Mark success after response is written
	s.markConnectionResult(conn.ID, model, result.StatusCode, nil)
}

func (s *Server) streamResponse(
	w http.ResponseWriter, r *http.Request,
	result *executor.Result,
	sourceFormat, targetFormat canonical.Format,
	model, requestID string,
	startTime time.Time,
	latencyTTFB time.Duration,
	providerID, connectionID string,
	reqCtx *requestContext,
) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if ok {
		flusher.Flush()
	}

	// Get translators
	targetTranslator, _ := s.deps.TranslateRegistry.Get(targetFormat)
	sourceTranslator, _ := s.deps.TranslateRegistry.Get(sourceFormat)

	upstreamState := translate.NewStreamState()
	clientState := translate.NewStreamState()

	scanner := bufio.NewScanner(result.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line

	var totalUsage *canonical.Usage

	for scanner.Scan() {
		line := scanner.Text()

		// Parse SSE line
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		// Upstream format → canonical chunks
		var chunks []canonical.Chunk
		if targetTranslator != nil && targetFormat != sourceFormat {
			var err error
			chunks, err = targetTranslator.StreamChunkToCanonical([]byte(data), upstreamState)
			if err != nil {
				slog.Error("stream translate error", "error", err)
				continue
			}
		} else {
			// Same format passthrough — just forward
			sse.WriteChunk(w, []byte(data))
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}

		// Canonical chunks → client format
		for _, chunk := range chunks {
			if chunk.Usage != nil {
				totalUsage = chunk.Usage
			}

			var outData []byte
			var err error
			if sourceTranslator != nil && sourceFormat != canonical.FormatOpenAI {
				outData, err = sourceTranslator.CanonicalToStreamChunk(chunk, clientState)
			} else {
				// Default: emit as OpenAI format
				openaiTranslator, _ := s.deps.TranslateRegistry.Get(canonical.FormatOpenAI)
				if openaiTranslator != nil {
					outData, err = openaiTranslator.CanonicalToStreamChunk(chunk, clientState)
				}
			}

			if err != nil {
				slog.Error("client stream translate error", "error", err)
				continue
			}
			if outData == nil {
				continue
			}

			// Handle multi-event output (Claude can emit multiple events per chunk)
			for _, eventData := range splitEvents(outData) {
				sse.WriteChunk(w, eventData)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}

	// Write [DONE]
	sse.WriteDone(w)
	if flusher != nil {
		flusher.Flush()
	}

	// Track usage
	s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, totalUsage, startTime, "success")

	// Post-response hooks: session affinity, conversation store, bridge lifecycle
	s.postRequestHook(reqCtx, providerID, model, totalUsage)
}

func (s *Server) forwardResponse(
	w http.ResponseWriter,
	result *executor.Result,
	sourceFormat, targetFormat canonical.Format,
	model, requestID string,
	startTime time.Time,
	latencyTTFB time.Duration,
	providerID, connectionID string,
	reqCtx *requestContext,
) {
	// Read full response
	respBody, err := io.ReadAll(result.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	// Extract usage for tracking (best-effort)
	usageData := extractResponseUsage(respBody)

	// If formats match, pass through directly
	if sourceFormat == targetFormat {
		copyRateLimitHeaders(w, result.Headers)
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(result.StatusCode)
		w.Write(respBody)
		s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, usageData, startTime, "success")
		s.postRequestHook(reqCtx, providerID, model, usageData)
		return
	}

	// Format translation: upstream response format → client response format
	clientBody, err := translateResponseBody(respBody, targetFormat, sourceFormat, model)
	if err != nil {
		slog.Warn("response translation failed, passing through", "error", err)
		clientBody = respBody
	}

	copyRateLimitHeaders(w, result.Headers)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	w.Write(clientBody)
	s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, usageData, startTime, "success")
	s.postRequestHook(reqCtx, providerID, model, usageData)
}

// extractResponseUsage attempts to extract usage data from a JSON response body.
func extractResponseUsage(body []byte) *canonical.Usage {
	// Try OpenAI shape: { "usage": { "prompt_tokens": N, "completion_tokens": N, "total_tokens": N } }
	var openaiProbe struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &openaiProbe) == nil && openaiProbe.Usage != nil {
		return &canonical.Usage{
			PromptTokens:     openaiProbe.Usage.PromptTokens,
			CompletionTokens: openaiProbe.Usage.CompletionTokens,
			TotalTokens:      openaiProbe.Usage.TotalTokens,
		}
	}

	// Try Claude shape: { "usage": { "input_tokens": N, "output_tokens": N } }
	var claudeProbe struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &claudeProbe) == nil && claudeProbe.Usage != nil {
		total := claudeProbe.Usage.InputTokens + claudeProbe.Usage.OutputTokens
		return &canonical.Usage{
			PromptTokens:     claudeProbe.Usage.InputTokens,
			CompletionTokens: claudeProbe.Usage.OutputTokens,
			TotalTokens:      total,
		}
	}

	return nil
}

// copyRateLimitHeaders forwards rate-limit headers from upstream.
func copyRateLimitHeaders(w http.ResponseWriter, upstream http.Header) {
	for _, h := range []string{
		"X-Ratelimit-Limit", "X-Ratelimit-Remaining", "X-Ratelimit-Reset", "Retry-After",
	} {
		if v := upstream.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

func (s *Server) handleComboRequest(
	w http.ResponseWriter, r *http.Request,
	body []byte,
	sourceFormat canonical.Format,
	comboModels []string,
	stream bool,
	requestID string,
	startTime time.Time,
	apiKeyID string,
) {
	for _, modelStr := range comboModels {
		providerID, model, _, _ := s.resolveModel(modelStr, body)
		conn, _, err := s.selectConnection(providerID, model, nil)
		if err != nil {
			slog.Info("combo skip", "model", modelStr, "error", err)
			continue
		}

		// Try this combo entry
		s.executeRequest(w, r, body, sourceFormat, providerID, model, stream, conn, nil, requestID, startTime, apiKeyID)
		return
	}

	writeError(w, http.StatusServiceUnavailable, "all combo models exhausted")
}

// handleListModels returns available models.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	connections, err := s.deps.Store.ListConnections(store.ConnectionFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list connections")
		return
	}

	// Build model list from active connections
	type modelEntry struct {
		ID       string `json:"id"`
		Object   string `json:"object"`
		Created  int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	seen := map[string]bool{}
	var models []modelEntry

	// Add models from connections
	for _, conn := range connections {
		if conn.State == "disabled" {
			continue
		}
		provDef := config.GetProvider(conn.Provider)
		if provDef == nil {
			continue
		}
		for _, m := range provDef.Models {
			fullID := conn.Provider + "/" + m
			if seen[fullID] {
				continue
			}
			seen[fullID] = true
			models = append(models, modelEntry{
				ID:       fullID,
				Object:   "model",
				Created:  time.Now().Unix(),
				OwnedBy: conn.Provider,
			})
		}
	}

	// Add combos
	combos, _ := s.deps.Store.ListCombos()
	for _, combo := range combos {
		models = append(models, modelEntry{
			ID:       combo.Name,
			Object:   "model",
			Created:  combo.CreatedAt.Unix(),
			OwnedBy: "sage-router",
		})
	}

	// Add aliases
	aliases, _ := s.deps.Store.ListAliases()
	for alias := range aliases {
		if !seen[alias] {
			models = append(models, modelEntry{
				ID:       alias,
				Object:   "model",
				Created:  time.Now().Unix(),
				OwnedBy: "sage-router",
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   models,
	})
}

// Helper types and functions
type ConnectionInfo struct {
	ID          string
	Credentials *executor.Credentials
	Endpoint    string
}

func (s *Server) resolveModel(model string, body []byte) (provider, resolvedModel string, isCombo bool, comboModels []string) {
	// Check smart routing (auto[:strategy])
	if strategy, isAuto := routing.ParseAutoModel(model); isAuto && s.deps.SmartRouter != nil {
		candidates := s.buildSmartCandidates()
		if len(candidates) > 0 {
			// Detect request constraints (Layer 2)
			constraints := detectRequestConstraints(body)

			// Extract first user message for session affinity
			firstMsg := extractFirstUserMsg(body)

			routeStart := time.Now()
			sorted := s.deps.SmartRouter.RouteWithConstraints(strategy, firstMsg, candidates, constraints)
			var models []string
			for _, c := range sorted {
				models = append(models, c.Provider+"/"+c.Model)
			}
			if len(models) > 0 {
				slog.Info("smart route", "strategy", string(strategy), "constraints", constraints, "candidates", len(models), "first", models[0])

				// Record routing telemetry
				affinityEntry := s.deps.SmartRouter.Affinity.Get(firstMsg)
				affinityHit := affinityEntry != nil
				affinityBreak := false
				if affinityEntry != nil && sorted[0].Provider+"/"+sorted[0].Model != affinityEntry.Provider+"/"+affinityEntry.Model {
					affinityBreak = true
				}
				go s.recordRoutingTelemetry(store.RoutingEntry{
					Strategy:       string(strategy),
					Provider:       sorted[0].Provider,
					Model:          sorted[0].Model,
					RoutingReason:  fmt.Sprintf("smart:%s", strategy),
					AffinityHit:    affinityHit,
					AffinityBreak:  affinityBreak,
					CandidateCount: len(candidates),
					FilteredCount:  len(candidates) - len(sorted),
					LatencyMs:      int(time.Since(routeStart).Milliseconds()),
					Status:         "ok",
				})

				return "", "", true, models
			}
		}
	}

	// Check combo
	combo, err := s.deps.Store.GetComboByName(model)
	if err == nil && combo != nil {
		return "", "", true, combo.Models
	}

	// Check alias
	target, err := s.deps.Store.GetAlias(model)
	if err == nil && target != "" {
		model = target
	}

	// Parse provider/model format
	if parts := strings.SplitN(model, "/", 2); len(parts) == 2 {
		return parts[0], parts[1], false, nil
	}

	// Try to find provider by model name
	return guessProvider(model), model, false, nil
}

// buildSmartCandidates builds the list of available models from active connections.
func (s *Server) buildSmartCandidates() []routing.ModelCandidate {
	connections, err := s.deps.Store.ListConnections(store.ConnectionFilter{})
	if err != nil {
		return nil
	}

	activeProviders := map[string]bool{}
	for _, conn := range connections {
		if conn.State != "disabled" {
			activeProviders[conn.Provider] = true
		}
	}

	var candidates []routing.ModelCandidate
	for _, m := range config.ModelCatalog {
		if activeProviders[m.Provider] {
			candidates = append(candidates, routing.ModelCandidate{
				Provider:         m.Provider,
				Model:            m.ID,
				Tier:             m.Tier,
				InputPrice:       m.InputPrice,
				ContextWindow:    m.ContextWindow,
				SupportsImages:   m.SupportsImages,
				SupportsTools:    m.SupportsTools,
				SupportsThinking: m.SupportsThinking,
			})
		}
	}
	return candidates
}

// selectConnection picks a connection and marks it Active. Returns retryAfterSec > 0
// when all connections are rate-limited.
func (s *Server) selectConnection(providerID, model string, excludeIDs []string) (*ConnectionInfo, int, error) {
	// Pre-selection: recover auto_detect connections stuck in AuthExpired
	s.recoverAutoDetectConnections(providerID)

	result, err := s.deps.ProviderSelector.Select(providerID, model, excludeIDs)
	if err != nil {
		var retryAfter int
		if result != nil && result.AllRateLimited && !result.EarliestRetry.IsZero() {
			retryAfter = int(time.Until(result.EarliestRetry).Seconds()) + 1
			if retryAfter < 1 {
				retryAfter = 1
			}
		}
		return nil, retryAfter, err
	}
	if result.Connection == nil {
		return nil, 0, fmt.Errorf("no available connections")
	}

	conn := result.Connection

	// Mark connection as in-use (Idle → Active)
	if err := conn.MarkUsed(); err != nil {
		// Already grabbed by another goroutine — exclude and retry
		if excludeIDs == nil {
			excludeIDs = []string{}
		}
		return s.selectConnection(providerID, model, append(excludeIDs, conn.ID))
	}

	// Look up stored credentials
	storedConn, err := s.deps.Store.GetConnection(conn.ID)
	if err != nil {
		conn.MarkSuccess() // release back to idle
		return nil, 0, fmt.Errorf("connection %s not found in store: %w", conn.ID, err)
	}

	creds := &executor.Credentials{
		ConnectionID: conn.ID,
		AuthType:     storedConn.AuthType,
		AccessToken:  storedConn.AccessToken,
		APIKey:       storedConn.APIKey,
	}

	// For auto_detect connections, resolve credentials from the filesystem at request time.
	// The store doesn't hold the actual token — it's read fresh each time.
	if storedConn.AuthType == "auto_detect" {
		freshCreds := s.resolveAutoDetectCredentials(storedConn.Provider)
		if freshCreds != nil {
			creds.AccessToken = freshCreds.AccessToken
			creds.AuthType = "oauth" // upstream executor expects oauth for bearer token auth
		} else {
			conn.MarkSuccess() // release back to idle
			return nil, 0, fmt.Errorf("auto_detect credentials unavailable for %s", storedConn.Provider)
		}
	}

	return &ConnectionInfo{
		ID:          conn.ID,
		Credentials: creds,
	}, 0, nil
}

// recoverAutoDetectConnections checks if any auto_detect connections for the given provider
// are stuck in AuthExpired and resets them if fresh credentials are available.
func (s *Server) recoverAutoDetectConnections(providerID string) {
	conns := s.deps.ProviderSelector.AllConnections(providerID)
	for _, c := range conns {
		if c.AuthType == "auto_detect" && c.State() == provider.StateAuthExpired {
			// Check if fresh credentials are available
			freshCreds := s.resolveAutoDetectCredentials(providerID)
			if freshCreds != nil {
				if err := c.ResetCooldown(); err == nil {
					slog.Info("auto_detect connection recovered", "provider", providerID, "connection", c.ID)
				}
			}
		}
	}
}

// resolveAutoDetectCredentials reads credentials from the filesystem for auto_detect connections.
func (s *Server) resolveAutoDetectCredentials(provider string) *executor.Credentials {
	switch provider {
	case "anthropic":
		_, creds := detect.DetectClaude()
		if creds == nil || creds.AccessToken == "" {
			return nil
		}
		if creds.ExpiresAt.Before(time.Now()) {
			slog.Warn("auto_detect credentials expired", "provider", provider, "expires_at", creds.ExpiresAt)
			return nil
		}
		return &executor.Credentials{
			AuthType:    "oauth",
			AccessToken: creds.AccessToken,
		}
	default:
		return nil
	}
}

// markConnectionResult transitions the connection state based on the upstream outcome.
func (s *Server) markConnectionResult(connID, model string, statusCode int, err error) {
	conn := s.deps.ProviderSelector.ConnectionByID(connID)
	if conn == nil {
		return
	}

	switch {
	case err != nil:
		conn.MarkErrored(err)
	case statusCode == 429:
		conn.MarkRateLimited(model, conn.BackoffLevel()+1)
	case statusCode == 401 || statusCode == 403:
		conn.MarkAuthExpired()
	case statusCode >= 500:
		conn.MarkErrored(fmt.Errorf("upstream %d", statusCode))
	default:
		conn.MarkSuccess()
	}
}

func (s *Server) trackUsage(requestID, provider, model, connectionID, apiKeyID string, u *canonical.Usage, startTime time.Time, status string) {
	if s.deps.UsageTracker == nil {
		return
	}

	var inputTokens, outputTokens, totalTokens int
	if u != nil {
		inputTokens = u.PromptTokens
		outputTokens = u.CompletionTokens
		totalTokens = u.TotalTokens
		if totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
	}

	entry := &usage.Entry{
		RequestID:    requestID,
		Provider:     provider,
		Model:        model,
		ConnectionID: connectionID,
		APIKeyID:     apiKeyID,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
		Cost:         config.EstimateCost(provider, model, inputTokens, outputTokens),
		Latency:      time.Since(startTime),
		Status:       status,
		CreatedAt:    time.Now(),
	}

	s.deps.UsageTracker.Record(entry)
}

// recordRoutingTelemetry persists a routing decision to the database (best-effort, async).
func (s *Server) recordRoutingTelemetry(entry store.RoutingEntry) {
	entry.ID = generateRequestID() // reuse ID generator
	entry.RequestID = entry.ID
	if err := s.deps.Store.RecordRouting(&entry); err != nil {
		slog.Warn("failed to record routing telemetry", "error", err)
	}
}

// postRequestHook runs after a successful response to update session affinity,
// store conversation turns, and manage bridge lifecycle.
func (s *Server) postRequestHook(reqCtx *requestContext, providerID, model string, u *canonical.Usage) {
	if reqCtx == nil || reqCtx.firstMsg == "" {
		return
	}

	// 1. Update session affinity
	if s.deps.SmartRouter != nil {
		s.deps.SmartRouter.Affinity.Set(reqCtx.firstMsg, providerID, model)

		// Manage bridge lifecycle — decrement turns
		if entry := s.deps.SmartRouter.Affinity.Get(reqCtx.firstMsg); entry != nil && entry.BridgeActive {
			entry.BridgeTurnsLeft--
			if entry.BridgeTurnsLeft <= 0 {
				entry.BridgeActive = false
				entry.BridgeTurnsLeft = 0
				slog.Info("bridge expired", "provider", providerID, "model", model)
			}
		}
	}

	// 2. Store conversation turns (user request + assistant response placeholder)
	if s.deps.ConversationStore != nil {
		// Store user message
		userMsg := extractLastUserMsg(reqCtx.requestBody)
		if userMsg != "" {
			s.deps.ConversationStore.AddTurn(reqCtx.firstMsg, "user", userMsg, "")
		}

		// Store assistant response marker with token count
		assistantTokens := 0
		if u != nil {
			assistantTokens = u.CompletionTokens
		}
		s.deps.ConversationStore.AddTurn(reqCtx.firstMsg, "assistant",
			fmt.Sprintf("[%s/%s response: %d tokens]", providerID, model, assistantTokens),
			providerID+"/"+model,
		)
	}
}

// extractLastUserMsg returns the text of the last user message.
func extractLastUserMsg(body []byte) string {
	var probe struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &probe)

	for i := len(probe.Messages) - 1; i >= 0; i-- {
		if probe.Messages[i].Role == "user" {
			var text string
			if json.Unmarshal(probe.Messages[i].Content, &text) == nil {
				return text
			}
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(probe.Messages[i].Content, &blocks) == nil {
				var parts []string
				for _, b := range blocks {
					if b.Type == "text" {
						parts = append(parts, b.Text)
					}
				}
				return strings.Join(parts, "\n")
			}
			return ""
		}
	}
	return ""
}

func extractAPIKey(r *http.Request) string {
	// Check Authorization header
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		key := strings.TrimPrefix(auth, "Bearer ")
		if strings.HasPrefix(key, "sk-sage-") {
			return key
		}
	}

	// Check x-api-key header
	if key := r.Header.Get("X-API-Key"); key != "" {
		return key
	}

	// Check query parameter
	if key := r.URL.Query().Get("key"); key != "" {
		return key
	}

	return ""
}

func extractModelAndStream(body []byte) (string, bool) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.Unmarshal(body, &probe)
	return probe.Model, probe.Stream
}

// detectRequestConstraints scans the raw request body to determine
// what capabilities are needed (Layer 2 hard constraint filtering).
func detectRequestConstraints(body []byte) routing.RequestConstraints {
	var probe struct {
		Tools    json.RawMessage `json:"tools"`
		Thinking json.RawMessage `json:"thinking"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &probe)

	hasTools := len(probe.Tools) > 2 // "[]" = 2 bytes
	hasThinking := len(probe.Thinking) > 0 && string(probe.Thinking) != "null"
	hasImages := false
	estTokens := len(body) / 4 // rough estimate

	// Scan messages for image content blocks
	for _, m := range probe.Messages {
		content := string(m.Content)
		if strings.Contains(content, `"image"`) || strings.Contains(content, `"image_url"`) || strings.Contains(content, `"inline_data"`) {
			hasImages = true
			break
		}
	}

	return routing.DetectConstraints(hasImages, hasTools, hasThinking, estTokens)
}

// extractFirstUserMsg returns the text of the first user message for session affinity.
func extractFirstUserMsg(body []byte) string {
	var probe struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &probe)

	for _, m := range probe.Messages {
		if m.Role == "user" {
			var text string
			if json.Unmarshal(m.Content, &text) == nil {
				return text
			}
			// Array of content blocks
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" {
						return b.Text
					}
				}
			}
			return ""
		}
	}
	return ""
}

// extractBypassReq builds a lightweight bypass.Req from raw JSON for pattern matching.
func extractBypassReq(body []byte, model string) *bypass.Req {
	var probe struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools json.RawMessage `json:"tools"`
	}
	json.Unmarshal(body, &probe)

	req := &bypass.Req{
		Model:        model,
		HasTools:     len(probe.Tools) > 2, // "[]" = 2 bytes
		MessageCount: len(probe.Messages),
	}

	// Extract system text
	if len(probe.System) > 0 {
		var sysStr string
		if json.Unmarshal(probe.System, &sysStr) == nil {
			req.SystemText = sysStr
		} else {
			// Array of blocks
			var blocks []struct{ Text string `json:"text"` }
			if json.Unmarshal(probe.System, &blocks) == nil {
				for _, b := range blocks {
					req.SystemText += b.Text + " "
				}
			}
		}
	}

	// Also check for system role messages (OpenAI format)
	for _, m := range probe.Messages {
		if m.Role == "system" {
			var text string
			if json.Unmarshal(m.Content, &text) == nil {
				req.SystemText += text + " "
			}
		}
	}

	// Extract last user message
	for i := len(probe.Messages) - 1; i >= 0; i-- {
		if probe.Messages[i].Role == "user" {
			var text string
			if json.Unmarshal(probe.Messages[i].Content, &text) == nil {
				req.LastUserMsg = text
			} else {
				// Array of content blocks
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(probe.Messages[i].Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type == "text" {
							req.LastUserMsg += b.Text
						}
					}
				}
			}
			break
		}
	}

	return req
}

func guessProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude") || strings.Contains(m, "haiku") || strings.Contains(m, "sonnet") || strings.Contains(m, "opus"):
		return "anthropic"
	case strings.Contains(m, "gpt") || strings.Contains(m, "o1") || strings.Contains(m, "o3") || strings.Contains(m, "o4"):
		return "openai"
	case strings.Contains(m, "gemini"):
		return "gemini"
	case strings.Contains(m, "llama") || strings.Contains(m, "qwen") || strings.Contains(m, "mistral"):
		return "ollama"
	default:
		return "openai"
	}
}

func splitEvents(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\n")
	var result [][]byte
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, []byte(p))
		}
	}
	if len(result) == 0 {
		return [][]byte{data}
	}
	return result
}

// translateResponseBody converts a non-streaming response between provider formats.
func translateResponseBody(body []byte, from, to canonical.Format, model string) ([]byte, error) {
	if from == to {
		return body, nil
	}
	if from == canonical.FormatClaude && to == canonical.FormatOpenAI {
		return claudeResponseToOpenAI(body, model)
	}
	if from == canonical.FormatOpenAI && to == canonical.FormatClaude {
		return openaiResponseToClaude(body, model)
	}
	return body, nil
}

func claudeResponseToOpenAI(body []byte, model string) ([]byte, error) {
	var claude struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &claude); err != nil {
		return nil, fmt.Errorf("parse claude response: %w", err)
	}

	var text string
	for _, c := range claude.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}

	finishReason := "stop"
	if claude.StopReason == "max_tokens" {
		finishReason = "length"
	} else if claude.StopReason == "tool_use" {
		finishReason = "tool_calls"
	}

	m := claude.Model
	if m == "" {
		m = model
	}

	resp := map[string]any{
		"id":     claude.ID,
		"object": "chat.completion",
		"model":  m,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": finishReason,
		}},
	}

	if claude.Usage != nil {
		resp["usage"] = map[string]any{
			"prompt_tokens":     claude.Usage.InputTokens,
			"completion_tokens": claude.Usage.OutputTokens,
			"total_tokens":      claude.Usage.InputTokens + claude.Usage.OutputTokens,
		}
	}

	return json.Marshal(resp)
}

func openaiResponseToClaude(body []byte, model string) ([]byte, error) {
	var oai struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &oai); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}

	var content string
	if len(oai.Choices) > 0 {
		content = oai.Choices[0].Message.Content
	}

	stopReason := "end_turn"
	if len(oai.Choices) > 0 {
		switch oai.Choices[0].FinishReason {
		case "length":
			stopReason = "max_tokens"
		case "tool_calls":
			stopReason = "tool_use"
		}
	}

	m := oai.Model
	if m == "" {
		m = model
	}

	resp := map[string]any{
		"id":    oai.ID,
		"type":  "message",
		"role":  "assistant",
		"model": m,
		"content": []map[string]any{
			{"type": "text", "text": content},
		},
		"stop_reason": stopReason,
	}

	if oai.Usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  oai.Usage.PromptTokens,
			"output_tokens": oai.Usage.CompletionTokens,
		}
	}

	return json.Marshal(resp)
}

func generateRequestID() string {
	b := make([]byte, 12)
	_, _ = crypto_rand.Read(b)
	return fmt.Sprintf("req_%x", b)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "error",
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// matchModelPattern checks if a model string matches a comma-separated pattern list.
// Patterns can be exact matches or use provider/* wildcards.
// Examples: "anthropic/*", "openai/gpt-4.1,anthropic/*", "gemini/*,gpt-4o-mini"
func matchModelPattern(model, patterns string) bool {
	for _, p := range strings.Split(patterns, ",") {
		p = strings.TrimSpace(p)
		if p == "" || p == "*" {
			return true
		}
		if strings.HasSuffix(p, "/*") {
			prefix := strings.TrimSuffix(p, "/*")
			if strings.HasPrefix(model, prefix+"/") || model == prefix {
				return true
			}
		} else if p == model {
			return true
		}
	}
	return false
}
