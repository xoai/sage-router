package server

import (
	"bufio"
	"bytes"
	"context"
	crypto_rand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
	"sage-router/internal/bypass"
	"sage-router/internal/catalog"
	"sage-router/internal/config"
	"sage-router/internal/cost"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/routing"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	openairesp "sage-router/internal/translate/openai-responses"
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

	// Resolve provider and model. allowedModels passed for StrategyUserOrder
	// (cycle 20260516-routing-strategy-ux M3 C1 fold) — empty string when
	// authenticatedKey is nil (unauth path) silently no-ops the pre-sort.
	providerID, resolvedModel, isCombo, comboModels, connStrategy := s.resolveModel(r.Context(), model, body, safeAllowedModels(authenticatedKey))

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

	// M4 — does this request's API key opt into tool-output compression?
	compressionEnabled := authenticatedKey != nil && authenticatedKey.CompressionEnabled

	if isCombo {
		var comboKeyID string
		if authenticatedKey != nil {
			comboKeyID = authenticatedKey.ID
		}
		s.handleComboRequest(w, r, body, sourceFormat, comboModels, stream, requestID, startTime, comboKeyID, connStrategy, compressionEnabled)
		return
	}

	// Select connection. connStrategy is SelectDefault on this direct
	// (non-auto) path — an auto:* request resolves isCombo=true and is served
	// by handleComboRequest above; the strategy is threaded here for
	// consistency (cycle 20260521-m3-quota-tracking spec §4.2).
	conn, retryAfter, err := s.selectConnection(providerID, resolvedModel, nil, connStrategy)
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

	// Execute with fallback (M1 α refactor — value-returning; cycle 20260516-routing-strategy-ux).
	// Internal connection-level fallback handled in executeRequest's inner loop.
	reqCtx := &requestContext{
		firstMsg:           extractFirstUserMsg(body),
		requestBody:        body,
		apiKeyID:           apiKeyID,
		strategy:           connStrategy,
		compressionEnabled: compressionEnabled,
	}
	result, err := s.executeRequest(r.Context(), r, body, sourceFormat, providerID, resolvedModel, stream, conn, nil, requestID, startTime, reqCtx)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream error: %v", err))
		return
	}
	s.forwardResult(w, r, result, sourceFormat, providerID, resolvedModel, stream, requestID, startTime, reqCtx)
}

// requestContext carries metadata through the request lifecycle for post-response hooks.
type requestContext struct {
	firstMsg           string                  // first user message (session key)
	requestBody        []byte                  // raw request body (for conversation store)
	apiKeyID           string                  // authenticated API key ID (for usage tracking)
	servedConnID       string                  // SET by executeRequest as the inner connection-level fallback loop progresses; READ by forwardResult for routing-log/usage-track connection attribution. Reflects the connection that actually served (or last-attempted) the request — distinct from the caller's original conn passed in, which may have been excluded mid-loop. Cycle 20260516-routing-strategy-ux M1 α refactor.
	strategy           provider.SelectStrategy // connection-selection strategy for this request (cycle 20260521-m3-quota-tracking); read by executeRequest's connection-level fallback loop. Zero value is SelectDefault — the safe default for the nil-reqCtx fallback.
	tokensBefore       int                     // M4: tokenizer estimate of the request BEFORE compression. SET by executeRequest's Compress block (cycle 20260522-m4-compression T8); READ by trackUsage. 0 when compression did not run.
	compressionEnabled bool                    // M4: this request's API key opted into tool-output compression; READ by executeRequest's Compress block.
}

// executeRequest sends a request upstream and returns the executor.Result so the
// CALLER decides whether to forward (handleChatCompletions :180) or advance to the
// next candidate (handleComboRequest's M2 walk-on-5xx loop at :611-635). Connection-
// level fallback (multiple connections for the SAME model) happens INSIDE this loop
// — caller never sees a network-error or retryable-status from a connection that
// had a healthy peer available. The caller's level of fallback is MODEL-level
// (combos + auto:*), one layer above.
//
// Cycle 20260516-routing-strategy-ux M1 α refactor (per spec v5 + plan v2):
//   - Returns (*executor.Result, error) instead of writing directly to ResponseWriter.
//   - Inner connection-level fallback is now a loop (M-v3-4 fold), not recursive self-calls.
//   - ctx-cancel check between iterations (M-v3-4 fold).
//   - reqCtx.servedConnID populated before return so forwardResult attributes correctly.
func (s *Server) executeRequest(
	ctx context.Context, r *http.Request,
	body []byte,
	sourceFormat canonical.Format,
	providerID, model string,
	stream bool,
	conn *ConnectionInfo,
	excludeIDs []string,
	requestID string,
	startTime time.Time,
	reqCtx *requestContext,
) (*executor.Result, error) {
	if reqCtx == nil {
		// Defense: callers should always pass a non-nil reqCtx so post-response
		// hooks (session affinity, conversation store, usage tracking) work.
		// nil is treated as "no post-response hooks needed" — used by tests.
		reqCtx = &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body}
	}

	// Resolve the variant Executor for this (provider, auth_type) FIRST so
	// formatOf() can ask it for the target wire format. variantExec is also
	// the source of optional-interface queries throughout the request lifecycle
	// (Format, NeedsOAuthIdentity, ParseAuthError, PreflightCredentials).
	// Returns nil only when the Variants registry isn't wired (test fixtures
	// pre-M1); helpers handle nil-executor by returning safe defaults.
	variantExec := s.resolveVariantExec(providerID, conn)

	// Determine target format via the variant's Format() method.
	// Replaces resolveTargetFormat helper that hardcoded openai+subscription
	// → FormatResponses. M5.2 of cycle 20260517-provider-auth-variants.
	targetFormat := formatOf(variantExec, providerID)

	// Translate request: source → canonical → target (ONCE — body sent to upstream
	// is the same regardless of which connection serves it; only translate per-request).
	canonReq, targetBody, err := s.deps.TranslateRegistry.TranslateRequest(sourceFormat, targetFormat, body, translateOptsFor(variantExec, model, providerID, stream))
	if err != nil {
		// Cycle 20260517-provider-auth-variants M2.6.1: ErrToolsUnsupported
		// synthetic-422 arm REMOVED. M0.8 live evidence + predecessor's
		// tools_streaming.sse capture confirmed the codex backend ACCEPTS
		// tools (HTTP 200); the AC-T4 premise was wrong-path. Translation
		// errors now bubble up to a generic 500.
		return nil, fmt.Errorf("translation error: %w", err)
	}

	// Stage [Compress] (M4, cycle 20260522-m4-compression): shrink
	// low-signal tool-result content before the cache-hint stage. Opt-in
	// per API key, tool-result-only, gated on the request being large
	// relative to the model's context window. It mutates only TypeToolResult
	// content — disjoint from the cache-hint region (system blocks) by
	// construction — and re-serializes via FromCanonical, the cache-hint
	// idiom. A nil Compressor (subsystem failed to load) skips the stage.
	if s.deps.Compressor != nil && canonReq != nil && reqCtx.compressionEnabled {
		ctxWindow := 0
		if s.deps.Catalog != nil {
			if m, ok := s.deps.Catalog.Lookup(providerID, model); ok {
				ctxWindow = m.ContextWindow
			}
		}
		if cr := s.deps.Compressor.Compress(canonReq, ctxWindow); cr.Compressed {
			if tgt, ok := s.deps.TranslateRegistry.Get(targetFormat); ok {
				if rewritten, err := tgt.FromCanonical(canonReq, translateOptsFor(variantExec, model, providerID, stream)); err == nil {
					targetBody = rewritten
					// Record tokensBefore ONLY once the compressed body has
					// actually replaced targetBody — otherwise a FromCanonical
					// failure would send the uncompressed body upstream while
					// the usage row falsely claims a saving (Gate-3 m-3).
					reqCtx.tokensBefore = cr.TokensBefore
				}
			}
		}
	}

	// Stage ⑤b: Inject cache hints (cost optimization). Pass the chosen
	// connection's AuthType so Gemini-subscription's caching-bypass issue
	// (spec §M3.2) doesn't accidentally apply once Gemini caching ships.
	authTypeForCache := ""
	if conn != nil {
		if c := s.deps.ProviderSelector.ConnectionByID(conn.ID); c != nil {
			authTypeForCache = c.AuthType
		}
	}
	if canonReq != nil && cost.InjectCacheHints(canonReq, providerID, authTypeForCache) {
		// Re-serialize with cache hints applied. translateOptsFor preserves
		// variant-dependent flags (e.g. EmitOAuthIdentity) across the cache-hint
		// path — see C1 review-fold for why the inline-literal pattern was
		// silently broken for anthropic+subscription.
		if tgt, ok := s.deps.TranslateRegistry.Get(targetFormat); ok {
			if rewritten, err := tgt.FromCanonical(canonReq, translateOptsFor(variantExec, model, providerID, stream)); err == nil {
				targetBody = rewritten
			}
		}
	}

	// Stage ④+: Context bridge injection on model switch (ADR §29)
	if canonReq != nil && s.deps.ConversationStore != nil && s.deps.SmartRouter != nil {
		firstMsg := reqCtx.firstMsg
		if firstMsg == "" {
			firstMsg = extractFirstUserMsg(body)
		}
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
							// Re-serialize. translateOptsFor preserves variant-dependent
							// flags (e.g. EmitOAuthIdentity) across the bridge path
							// — see C1 review-fold.
							if tgt, ok := s.deps.TranslateRegistry.Get(targetFormat); ok {
								if rewritten, err := tgt.FromCanonical(canonReq, translateOptsFor(variantExec, model, providerID, stream)); err == nil {
									targetBody = rewritten
								}
							}
						}
					}
				}
			}
		}
	}

	// Connection-level fallback LOOP (M-v3-4 fold — was recursive self-calls at
	// pre-refactor :322 and :378). Each iteration tries one connection; on
	// network error or retryable status, advances to the next connection for
	// the SAME model. Returns on success OR exhaustion.
	if excludeIDs == nil {
		excludeIDs = []string{}
	}
	currentConn := conn
	if currentConn == nil {
		return nil, fmt.Errorf("nil connection passed to executeRequest")
	}

	for {
		// AC9: release this connection's HALF_OPEN trial slot on every terminal
		// path of the request. The selector claims the slot inside Select
		// (TryClaimHalfOpenTrial — spec §4); the request path is the single
		// authoritative releaser. ReleaseHalfOpenTrial is idempotent and a
		// no-op for a CLOSED connection that never claimed a slot, so deferring
		// it unconditionally is safe. A defer fires on every exit of this frame
		// — normal return, error return, context-cancel, and panic-unwind — so
		// one defer per connection covers all five terminal paths. The fallback
		// loop registers one per iteration; they accumulate and all fire when
		// executeRequest returns.
		if pc := s.deps.ProviderSelector.ConnectionByID(currentConn.ID); pc != nil {
			defer pc.ReleaseHalfOpenTrial()
		}

		// M-v3-4 fold: ctx-cancel check between iterations.
		if err := ctx.Err(); err != nil {
			reqCtx.servedConnID = currentConn.ID
			return nil, err
		}

		// Pick executor via the variant registry first; fall back to the
		// legacy Executors map for compatibility with test fixtures that
		// don't wire Variants. Cycle 20260517-provider-auth-variants M5.6
		// pulled forward to M2 — the variant dispatch is what makes
		// (openai, subscription) → CodexSubscriptionExecutor route to
		// chatgpt.com/backend-api/codex/responses; without it the
		// translator's FormatResponses body shape goes to api.openai.com.
		// Cycle 20260517-provider-auth-variants M5.6:
		// Legacy s.deps.Executors[providerID] fallback REMOVED. Variants
		// is the sole dispatch source post-M2. If no variant resolves,
		// the wildcard (provider, "") fallback inside Variants.Get
		// catches; if THAT also misses, the request fails fast (rather
		// than silently routing to "default" executor which masked the
		// wiring gap in the pre-rip era).
		authType := ""
		if currentConn != nil && currentConn.Credentials != nil {
			authType = currentConn.Credentials.AuthType
		}
		// M3 nil-guard (post-ship /review fold): Variants is set by main.go
		// at boot but a test fixture constructing Dependencies{} without it
		// would panic at the Get call. Fail fast with a clean error so
		// future test authors see the wiring gap, not a goroutine panic.
		if s.deps.Variants == nil {
			return nil, fmt.Errorf("no Variants registry wired (deps.Variants is nil)")
		}
		exec, ok := s.deps.Variants.Get(providerID, authType)
		if !ok || exec == nil {
			return nil, fmt.Errorf("no executor registered for (provider=%s, auth_type=%s)", providerID, authType)
		}

		// C1 fix (post-ship /review fold): wire the variant's preflight
		// BEFORE building/sending the upstream request. When a variant
		// implements PreflightChecker, it can synchronously reject
		// connections that lack viable credentials, returning a TYPED
		// error (e.g., ErrTierMissingScopes) that markConnectionResult
		// routes through SetLastError + MarkAuthExpired → dashboard's
		// friendly re-auth banner fires immediately. Without this wiring
		// the duplicate check inside CodexSubscriptionExecutor.Execute
		// still gates the HTTP call but produces a generic error → the
		// connection lands in StateErrored (not AuthExpired), losing the
		// friendly message and triggering retry semantics where AuthExpired
		// semantics belong.
		if preErr := preflightCreds(exec, currentConn.Credentials); preErr != nil {
			// statusCode=401 routes markConnectionResult through the
			// AuthExpired branch + parseAuthError (which our variant
			// returns the same typed error from). The body is nil because
			// no HTTP call happened — parseAuthError handles nil body.
			s.markConnectionResult(currentConn.ID, model, 401, nil, nil, 0)
			// Now set the LastError directly so dashboard surfaces the
			// friendly variant-supplied message (parseAuthError on a nil
			// body won't match the pattern; this is the explicit hook).
			if pc := s.deps.ProviderSelector.ConnectionByID(currentConn.ID); pc != nil {
				pc.SetLastError(preErr)
			}
			excludeIDs = append(excludeIDs, currentConn.ID)
			nextConn, _, nextErr := s.selectConnection(providerID, model, excludeIDs, reqCtx.strategy)
			if nextErr != nil {
				reqCtx.servedConnID = currentConn.ID
				return nil, preErr
			}
			currentConn = nextConn
			continue
		}

		// Execute upstream call with per-attempt 5min timeout (preserved from
		// pre-refactor behavior). The cancel func is bound to one of three
		// disposition paths below:
		//   1. Network error → fire cancel directly (result is nil, nothing
		//      to drain); advance or return.
		//   2. HTTP error → drain body, close, fire cancel directly; advance
		//      or wrap respBody into a fresh NopCloser + return.
		//   3. Success → DO NOT fire cancel here. Stream/forward path must
		//      drain result.Body lazily; firing cancel now would tear down
		//      the live upstream connection mid-stream (regression observed
		//      in production: 1-3 chars then EOF). Cancel ownership is
		//      transferred to the caller via cancelOnClose wrapper — when
		//      forwardResult closes the body (after streamResponse or
		//      forwardResponse drains it), cancel fires too.
		upstreamCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		result, execErr := exec.Execute(upstreamCtx, &executor.ExecuteRequest{
			Model:       model,
			Body:        targetBody,
			Stream:      stream,
			Credentials: currentConn.Credentials,
			Endpoint:    currentConn.Endpoint,
		})

		// Network/transport error → mark + try next connection.
		if execErr != nil {
			cancel() // safe: result is nil, no live stream
			slog.Error("upstream error", "provider", providerID, "connection", currentConn.ID, "error", execErr)
			s.markConnectionResult(currentConn.ID, model, 0, nil, execErr, 0)
			excludeIDs = append(excludeIDs, currentConn.ID)
			nextConn, _, nextErr := s.selectConnection(providerID, model, excludeIDs, reqCtx.strategy)
			if nextErr != nil {
				reqCtx.servedConnID = currentConn.ID
				return nil, execErr // exhausted — caller decides (M2 may advance to next combo member)
			}
			slog.Info("falling back", "provider", providerID, "from", currentConn.ID, "to", nextConn.ID)
			currentConn = nextConn
			continue
		}

		// HTTP-level error (4xx/5xx) → mark + maybe try next connection.
		if result.StatusCode >= 400 {
			respBody, _ := io.ReadAll(result.Body)
			result.Body.Close()
			cancel() // safe: body fully drained
			statusCode := result.StatusCode
			// Mark connection state based on error. Pass respBody so 401/403 can
			// detect model-level rejections (vs. token-level rejections) and
			// add the model to the per-connection denylist accordingly.
			// retryAfter: parse the upstream's rate-limit headers so a 429's
			// breaker cooldown honors Retry-After (§5). ParseRateLimitReset
			// yields a zero Reset for non-rate-limit responses — harmless to
			// compute for every 4xx/5xx; only the 429 branch consumes it.
			rl := executor.ParseRateLimitReset(providerID, result.Headers, time.Now())
			s.markConnectionResult(currentConn.ID, model, statusCode, respBody, nil, rl.Reset)
			// M3: refresh the connection's QuotaWindow from this response's
			// rate-limit headers — a separate parse from the breaker's rl above
			// (the accepted two-parse on the error path; spec §3). Covers every
			// 4xx/5xx, including an intermediate one the fallback loop walks past.
			s.applyQuotaWindow(currentConn.ID, providerID, result.Headers)

			// Models Discovery M2.7 — on-404 ad-hoc refresh (AC16).
			// Upstream "model not found" usually means the catalog is stale.
			// Fire-and-forget; the helper's own gates (backoff + 5-min
			// debounce) keep request-flood scenarios from hammering /v1/models.
			if statusCode == http.StatusNotFound && s.deps.Discovery != nil && s.deps.CatalogStore != nil {
				go s.triggerOnNotFoundDiscovery(providerID, currentConn)
			}

			// Retryable status → try next connection (connection-level fallback).
			if executor.IsFallbackEligible(statusCode) {
				excludeIDs = append(excludeIDs, currentConn.ID)
				nextConn, _, nextErr := s.selectConnection(providerID, model, excludeIDs, reqCtx.strategy)
				if nextErr == nil {
					slog.Info("falling back on error",
						"provider", providerID, "from", currentConn.ID, "to", nextConn.ID,
						"status", statusCode,
					)
					currentConn = nextConn
					continue
				}
			}

			// Return error-status Result. respBody is already drained, so wrap
			// it in a fresh ReadCloser for the caller. Caller (handleComboRequest
			// after M2 walk fold OR handleChatCompletions's forwardResult) decides
			// forward-to-client vs advance-to-next-candidate.
			reqCtx.servedConnID = currentConn.ID
			return &executor.Result{
				StatusCode: statusCode,
				Headers:    result.Headers,
				Body:       io.NopCloser(bytes.NewReader(respBody)),
				URL:        result.URL,
				Latency:    result.Latency,
			}, nil
		}

		// Success — return Result for caller to forward via forwardResult.
		// Transfer upstreamCtx cancel ownership to the body wrapper: when
		// forwardResult closes the body after streamResponse/forwardResponse
		// drains the upstream stream, cancel fires too. This prevents the
		// mid-stream truncation bug observed in production where firing
		// cancel() before the caller drained the body tore down the live
		// HTTP connection, delivering only what was already buffered.
		// markConnectionResult-success happens post-forward in forwardResult
		// (matches pre-refactor :399 semantics).
		// M3: refresh the connection's QuotaWindow from the response headers —
		// the hot path's single parse (spec §3).
		s.applyQuotaWindow(currentConn.ID, providerID, result.Headers)
		result.Body = &cancelOnClose{ReadCloser: result.Body, cancel: cancel}
		reqCtx.servedConnID = currentConn.ID
		return result, nil
	}
}

// cancelOnClose wraps an io.ReadCloser so that calling Close() also fires the
// associated context.CancelFunc. Used by executeRequest to transfer ownership
// of the per-attempt timeout context to the caller: caller drains the body
// (lazily for streaming), then closes — at which point we cancel the upstream
// context to release the http.Transport resources. Without this, firing cancel
// immediately after exec.Execute returns would close the live HTTP connection
// before the caller's stream/forward path drains response chunks, truncating
// SSE responses to whatever was already buffered (regression: 1-3 chars then
// EOF, reported 2026-05-16). Cycle 20260516-routing-strategy-ux M1 hotfix.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	if c.cancel != nil {
		c.cancel()
	}
	return err
}

// forwardResult writes the Result returned by executeRequest to the client's
// ResponseWriter. Splits to streamResponse or forwardResponse based on stream
// flag, then markConnectionResult-success on the actual-served connection (read
// from reqCtx.servedConnID).
//
// For result.StatusCode >= 400, forwardResult writes the upstream error body
// directly (no canonical/target translation — error bodies vary widely and the
// upstream is the authoritative source for the error message).
//
// Cycle 20260516-routing-strategy-ux M1 α refactor.
func (s *Server) forwardResult(
	w http.ResponseWriter, r *http.Request,
	result *executor.Result,
	sourceFormat canonical.Format,
	providerID, model string,
	stream bool,
	requestID string,
	startTime time.Time,
	reqCtx *requestContext,
) {
	if reqCtx == nil {
		reqCtx = &requestContext{}
	}
	connID := reqCtx.servedConnID

	// Body lifecycle: caller (this function) owns Close. For success paths
	// the Body is a cancelOnClose wrapper, so Close also fires the per-attempt
	// upstream context cancel. Pre-M1 the equivalent `defer result.Body.Close()`
	// lived at the bottom of executeRequestWithCtx; moving it here matches the
	// new "caller owns the Body" contract introduced by M1.
	defer result.Body.Close()

	if result.StatusCode >= 400 {
		// Forward upstream error body directly to client. Body is already
		// re-wrapped as NopCloser(bytes.NewReader) inside executeRequest.
		respBody, _ := io.ReadAll(result.Body)
		copyRateLimitHeaders(w, result.Headers)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", requestID)
		w.WriteHeader(result.StatusCode)
		w.Write(respBody)
		return
	}

	// M5.2 fold: forwardResult only has connID; look up the variant via
	// the ProviderSelector → connInfo path that formatOf needs. When the
	// connection isn't registered (race window during create), fall back
	// to the provider's natural target format.
	var fwdVariantExec executor.Executor
	if s.deps.ProviderSelector != nil {
		if pc := s.deps.ProviderSelector.ConnectionByID(connID); pc != nil && s.deps.Variants != nil {
			if v, ok := s.deps.Variants.Get(providerID, pc.AuthType); ok {
				fwdVariantExec = v
			}
		}
	}
	targetFormat := formatOf(fwdVariantExec, providerID)
	latencyTTFB := time.Since(startTime)

	if stream {
		s.streamResponse(w, r, result, sourceFormat, targetFormat, model, requestID, startTime, latencyTTFB, providerID, connID, reqCtx)
	} else {
		s.forwardResponse(w, result, sourceFormat, targetFormat, model, requestID, startTime, latencyTTFB, providerID, connID, reqCtx)
	}

	// Mark success after response is written (preserved from pre-refactor :399).
	s.markConnectionResult(connID, model, result.StatusCode, nil, nil, 0)
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

	// M2 fix (post-ship /review fold): surface scanner errors instead
	// of silently truncating. bufio.Scanner errors when (a) a single
	// SSE line exceeds the buffer cap (a malicious or buggy upstream
	// could emit very large events) OR (b) the upstream stream errors
	// mid-flight. Pre-fix: the for-loop exit silently followed by
	// WriteDone made the client see a complete-looking response.
	if serr := scanner.Err(); serr != nil {
		slog.Error("stream scanner error — emitting client-visible error chunk",
			"provider", providerID, "connection", connectionID, "err", serr)
		// Emit an SSE error event the client can detect (matches OpenAI
		// chat-completions error shape). Don't WriteDone — the [DONE]
		// terminator implies success.
		errChunk := []byte(`{"error":{"message":"upstream stream truncated","type":"server_error"}}`)
		sse.WriteChunk(w, errChunk)
		if flusher != nil {
			flusher.Flush()
		}
		s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, totalUsage, startTime, "error", reqCtx.tokensBefore)
		return
	}

	// Write [DONE]
	sse.WriteDone(w)
	if flusher != nil {
		flusher.Flush()
	}

	// Track usage
	s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, totalUsage, startTime, "success", reqCtx.tokensBefore)

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
		s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, usageData, startTime, "success", reqCtx.tokensBefore)
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
	s.trackUsage(requestID, providerID, model, connectionID, reqCtx.apiKeyID, usageData, startTime, "success", reqCtx.tokensBefore)
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
	connStrategy provider.SelectStrategy,
	compressionEnabled bool,
) {
	reqCtx := &requestContext{
		firstMsg:           extractFirstUserMsg(body),
		requestBody:        body,
		apiKeyID:           apiKeyID,
		strategy:           connStrategy,
		compressionEnabled: compressionEnabled,
	}

	// Track last 5xx status across walk iterations for the "all exhausted" return
	// (AC-F12e): when every candidate returns 5xx, propagate the last 5xx to the
	// client instead of a generic 503.
	lastWalkStatus := 0

	for _, modelStr := range comboModels {
		// M2.3 fold: ctx-cancel check between candidates — abort walk if client gone.
		if err := r.Context().Err(); err != nil {
			slog.Info("combo walk aborted", "reason", "ctx canceled", "error", err)
			return
		}

		// M2.4 fold: auto:* recursion guard. A combo member that itself starts
		// with "auto:" would recursively expand into another smart-route
		// candidate list. Skip such members to avoid surprising recursion.
		if strings.HasPrefix(modelStr, "auto:") {
			slog.Warn("combo skip auto:* member (recursion guard)", "model", modelStr)
			continue
		}

		// Combo member resolution: allowedModels="" — combo IS the routing.
		// Edge case (cycle 20260516-routing-strategy-ux M3 C-plan-3): if
		// modelStr is "auto:user-order", the pre-sort inside resolveModel
		// gets allowedModels="" and silently no-ops. The M2.4 auto:* recursion
		// guard above already skips such members before reaching this call,
		// so this is defense-in-depth.
		// The 5th return (the member's own connStrategy) is discarded: combo
		// members are plain provider/model (the auto:* recursion guard above
		// skips auto members), so it is always SelectDefault. The request-level
		// strategy — reqCtx.strategy, set from the top-level connStrategy — is
		// what applies (cycle 20260521-m3-quota-tracking spec §4.2).
		providerID, model, _, _, _ := s.resolveModel(r.Context(), modelStr, body, "")
		conn, _, err := s.selectConnection(providerID, model, nil, reqCtx.strategy)
		if err != nil {
			slog.Info("combo skip", "model", modelStr, "error", err)
			continue
		}

		// M1 α refactor — value-returning executeRequest; caller forwards via
		// forwardResult. M2 extends to walk on 5xx (advance to next combo
		// member). 4xx still forwards (client error, not "model broken").
		result, execErr := s.executeRequest(r.Context(), r, body, sourceFormat, providerID, model, stream, conn, nil, requestID, startTime, reqCtx)
		if execErr != nil {
			// Network error with exhausted connection-level fallback: advance to
			// next combo member.
			slog.Info("combo executor exhausted", "model", modelStr, "error", execErr)
			continue
		}

		// M2.2 fold (AC-F9..F12): walk on 5xx — close body explicitly (AC-X6)
		// to prevent fd leak, then advance to next candidate.
		if result.StatusCode >= 500 {
			if result.Body != nil {
				result.Body.Close() // AC-X6: explicit close before walk-advance.
			}
			lastWalkStatus = result.StatusCode
			slog.Info("combo walk on 5xx", "model", modelStr, "status", result.StatusCode)
			continue
		}

		// M5.2 (cycle 20260517-openai-subscription-responses-api):
		// walk on HTTP 422 ONLY when the body indicates an
		// `unsupported_feature` (the sentinel error openairesp.Translator
		// emits when FromCanonical sees Tools — AC-T4). Other 422s (e.g.,
		// model-not-found, validation errors that ARE the client's fault)
		// fall through to forwardResult so the client sees the error.
		// Walking on `unsupported_feature` lets a combo like
		// [openai/gpt-5, anthropic/claude-3-7] succeed via anthropic for
		// tool requests when openai+subscription declines them.
		if result.StatusCode == 422 {
			body, _ := io.ReadAll(result.Body)
			result.Body.Close()
			// Tighter pin than substring-match per reviewer M4-note: an
			// upstream validation error referencing the field name
			// "unsupported_feature" would false-walk under a loose
			// substring. Match the JSON key pattern instead.
			if bytes.Contains(body, []byte(`"type":"unsupported_feature"`)) {
				lastWalkStatus = 422
				slog.Info("combo walk on 422 unsupported_feature", "model", modelStr)
				continue
			}
			// Other 422s: restore body for forwardResult and fall through.
			result.Body = io.NopCloser(bytes.NewReader(body))
		}

		// 4xx (client error) and 2xx (success): forward to client and return.
		// 4xx does NOT advance to next candidate per AC-F12d.
		s.forwardResult(w, r, result, sourceFormat, providerID, model, stream, requestID, startTime, reqCtx)
		return
	}

	// All candidates exhausted. If any returned 5xx, propagate that status
	// (AC-F12e). Otherwise (selectConnection-failures or auto:* skips), 503.
	if lastWalkStatus > 0 {
		writeError(w, lastWalkStatus, fmt.Sprintf("all combo candidates exhausted; last upstream status %d", lastWalkStatus))
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
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
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
				ID:      fullID,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: conn.Provider,
			})
		}
	}

	// Add combos
	combos, _ := s.deps.Store.ListCombos()
	for _, combo := range combos {
		models = append(models, modelEntry{
			ID:      combo.Name,
			Object:  "model",
			Created: combo.CreatedAt.Unix(),
			OwnedBy: "sage-router",
		})
	}

	// Add aliases
	aliases, _ := s.deps.Store.ListAliases()
	for alias := range aliases {
		if !seen[alias] {
			models = append(models, modelEntry{
				ID:      alias,
				Object:  "model",
				Created: time.Now().Unix(),
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

// safeAllowedModels extracts the allowed_models string from an *store.APIKey
// pointer, returning "" for nil. Used by resolveModel callers to pass user-
// order context without nil-checking inline.
// Cycle 20260516-routing-strategy-ux M3.
func safeAllowedModels(key *store.APIKey) string {
	if key == nil {
		return ""
	}
	return key.AllowedModels
}

// sortByAllowedModelsOrder rearranges candidates to match the position-order of
// the API key's allowed_models comma-string. Exact matches beat wildcard matches
// (AC-F8 — exact "anthropic/claude-x" at position 1 ranks AFTER wildcard
// "anthropic/*" at position 0 only if claude-x itself isn't claude-x; same-id
// candidates always rank by their exact-match position). Bare "*" entries are
// ignored for ordering (they mean "match all", not a position). Candidates not
// present in allowed_models fall to the end gracefully.
// Cycle 20260516-routing-strategy-ux M3 (spec L300-340).
func sortByAllowedModelsOrder(candidates []routing.ModelCandidate, allowedModels string) []routing.ModelCandidate {
	exactPositions := map[string]int{}
	wildcardPositions := map[string]int{}
	for i, entry := range strings.Split(allowedModels, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" || entry == "*" {
			continue // AC-F7: bare * ignored
		}
		if strings.HasSuffix(entry, "/*") {
			wildcardPositions[strings.TrimSuffix(entry, "/*")] = i
		} else {
			exactPositions[entry] = i
		}
	}
	positionForCandidate := func(provider, model string) (int, bool) {
		// AC-F8: exact match beats wildcard.
		if pos, ok := exactPositions[provider+"/"+model]; ok {
			return pos, true
		}
		if pos, ok := wildcardPositions[provider]; ok {
			return pos, true
		}
		return 0, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		posI, hasI := positionForCandidate(candidates[i].Provider, candidates[i].Model)
		posJ, hasJ := positionForCandidate(candidates[j].Provider, candidates[j].Model)
		if hasI && !hasJ {
			return true
		}
		if !hasI && hasJ {
			return false
		}
		if hasI && hasJ {
			return posI < posJ
		}
		return false // both unmatched: preserve input order (AC-F6 graceful fall-to-end)
	})
	return candidates
}

// connStrategyFor maps a routing.Strategy to the provider.SelectStrategy that
// orders connection candidates. Only the two connection-selection strategies
// (cycle 20260521-m3-quota-tracking) carry a non-default mapping; every other
// routing strategy — and any unrecognized value — selects connections by the
// default order.
func connStrategyFor(strategy routing.Strategy) provider.SelectStrategy {
	switch strategy {
	case routing.StrategyP2C:
		return provider.SelectP2C
	case routing.StrategyResetAware:
		return provider.SelectResetAware
	default:
		return provider.SelectDefault
	}
}

// resolveModel resolves a request's `model` field into a (provider, model) pair
// or a combo's member list. The allowedModels parameter (added by cycle
// 20260516-routing-strategy-ux C1 fold) carries the API key's allowed_models
// comma-string used by StrategyUserOrder to pre-sort smart-route candidates by
// user position before Route() runs.
//
// connStrategy (cycle 20260521-m3-quota-tracking) is the connection-selection
// strategy the request resolved to: SelectP2C / SelectResetAware for an
// auto:p2c / auto:reset-aware smart route, SelectDefault for every other path
// (plain model, alias, combo, the other five smart-route strategies).
func (s *Server) resolveModel(ctx context.Context, model string, body []byte, allowedModels string) (providerID, resolvedModel string, isCombo bool, comboModels []string, connStrategy provider.SelectStrategy) {
	// Check smart routing (auto[:strategy])
	if strategy, isAuto := routing.ParseAutoModel(model); isAuto && s.deps.SmartRouter != nil {
		candidates := s.buildSmartCandidates(ctx, strategy)
		if len(candidates) > 0 {
			// M3.6 (cycle 20260516-routing-strategy-ux): user-order pre-sort.
			// No dependency on constraints/firstMsg — apply directly after
			// buildSmartCandidates so the no-op StrategyUserOrder sortByStrategy
			// case at routing/router.go preserves this order via stable-sort.
			// Note: session-affinity hits in RouteWithConstraints below still
			// override user-order — see AC-H7 in spec.md. User-order applies on
			// affinity-miss (first session request OR post-TTL).
			if strategy == routing.StrategyUserOrder && allowedModels != "" {
				candidates = sortByAllowedModelsOrder(candidates, allowedModels)
			}

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

				return "", "", true, models, connStrategyFor(strategy)
			}
		}
	}

	// Check combo
	combo, err := s.deps.Store.GetComboByName(model)
	if err == nil && combo != nil {
		return "", "", true, combo.Models, provider.SelectDefault
	}

	// Check alias
	target, err := s.deps.Store.GetAlias(model)
	if err == nil && target != "" {
		model = target
	}

	// Parse provider/model format
	if parts := strings.SplitN(model, "/", 2); len(parts) == 2 {
		return parts[0], parts[1], false, nil, provider.SelectDefault
	}

	// Try to find provider by model name
	return guessProvider(model), model, false, nil, provider.SelectDefault
}

// pickSampleConnByProvider (Models Discovery M3.4a, NM-r3-6 fix)
// returns a map[provider]connection_id picking the lowest-ID active
// connection per provider. Used by M3.4b to consult
// `Store.GetCacheHitRate` with a STABLE sample per provider — without
// a deterministic pick, the cheap-strategy `effectivePrice` ranking
// would swing between requests as `ListConnections` ordering varies.
//
// Pure function (no Server receiver) so tests can exercise it
// directly with a shuffled slice and assert order-independence.
// Returns an empty (not nil) map for safe indexing.
func pickSampleConnByProvider(connections []store.Connection) map[string]string {
	out := map[string]string{}
	for _, c := range connections {
		if c.State == "disabled" {
			continue
		}
		cur, have := out[c.Provider]
		if !have || c.ID < cur {
			out[c.Provider] = c.ID
		}
	}
	return out
}

// buildSmartCandidates builds the list of available models from active connections.
//
// For each candidate, HasSubscriptionConnection is true when the user has
// at least one non-disabled subscription connection for the model's
// provider AND that subscription tier permits the model. The smart
// router's StrategyCheap branch uses this to prefer zero-marginal-cost
// candidates over priced ones.
//
// Models Discovery M3.4a — connections are sorted by ID before the
// pass that builds activeProviders + providersWithSubscription +
// sampleConnByProvider. The sort is for deterministic sample-
// connection selection (consumed by M3.4b's cache-hit-rate
// enrichment); activeProviders / providersWithSubscription are
// set-based so iteration order doesn't matter to them, but the sort
// is cheap and makes the loop self-consistent for future readers.
//
// Models Discovery M3.4b — two enrichment passes added:
//
//  1. Capability override: when the provider's executor implements
//     `executor.CapabilityOverrider`, the catalog's capability flags
//     are passed through `OverrideCapabilities(modelID, base)` and
//     the returned struct replaces the candidate's flags. Catches
//     newer model variants whose seed row hasn't been updated yet.
//
//  2. Cache-hit-rate enrichment: under `StrategyCheap` and only for
//     models with non-zero `CacheRead` pricing, the per-connection
//     24h cache-hit-rate is queried via `Store.GetCacheHitRate` using
//     the deterministic `sampleConnByProvider` pick. The result feeds
//     `routing.effectivePrice` for cache-aware ranking (AC26). Other
//     strategies skip the query (zero SQL hits on the hot path).
//
// `ctx` is propagated to the cache-hit-rate query so request
// cancellation flows through. `strategy` is needed to gate the query.
func (s *Server) buildSmartCandidates(ctx context.Context, strategy routing.Strategy) []routing.ModelCandidate {
	connections, err := s.deps.Store.ListConnections(store.ConnectionFilter{})
	if err != nil {
		return nil
	}

	// Sort by ID for determinism (M3.4a). `ListConnections` returns
	// rows ordered by `priority ASC, name ASC` per sqlite.go, but
	// M3.4b's cache-hit-rate path needs a stable sample regardless
	// of priority/name churn. Sorting here means pickSampleConnByProvider
	// can scan in order; it could also use `c.ID < cur` independently
	// (which it does), so the sort is belt-and-suspenders.
	sort.Slice(connections, func(i, j int) bool {
		return connections[i].ID < connections[j].ID
	})

	activeProviders := map[string]bool{}
	providersWithSubscription := map[string]bool{}
	for _, conn := range connections {
		if conn.State == "disabled" {
			continue
		}
		activeProviders[conn.Provider] = true
		if conn.AuthType == auth.AuthTypeSubscription {
			providersWithSubscription[conn.Provider] = true
		}
	}

	// M3.4b — sample connection per provider (lowest-ID active),
	// used downstream by the cache-hit-rate query so the cheap
	// ranking is stable across requests.
	sampleConn := pickSampleConnByProvider(connections)

	// AC-A6 (C2 fold): build provider → AuthType for the sample
	// connection. Variant-aware capability resolution at L1229 uses
	// this to pick (provider, auth_type) → variant Executor — so
	// CapabilityOverrider on the SUBSCRIPTION variant (e.g.,
	// ClaudeMaxExecutor's thinking-via-interleaved-beta) overrides
	// catalog flags correctly when both apikey + subscription
	// connections exist for the same provider.
	sampleAuthType := map[string]string{}
	for _, c := range connections {
		if c.State == "disabled" {
			continue
		}
		if sampleConn[c.Provider] == c.ID {
			sampleAuthType[c.Provider] = c.AuthType
		}
	}

	// M3.4b post-review fix — memo GetCacheHitRate results by
	// connID across the catalog iteration. The query input
	// (connID, lookback) is constant per provider, so without
	// memoization N models × K providers issue N×K identical SQL
	// hits per StrategyCheap request. With the memo, exactly one
	// query per active provider fires regardless of catalog size.
	// `ok` is tracked alongside the value so we don't re-query
	// after a transient SQL error (we cache the zero result too).
	type cacheRatioCache struct {
		ratio  float64
		cached bool
	}
	hitRateByConn := map[string]cacheRatioCache{}

	// Iterate the runtime catalog (Models Discovery M1.9a — rewires
	// the static model map read to catalog.Registry). The catalog
	// Store guarantees ORDER BY provider, model_id ASC (AC11b), so
	// iteration order is deterministic across runs — preserves the
	// M1.9-baseline golden snapshot.
	if s.deps.Catalog == nil {
		// Defensive fallback for tests / partial wiring. Production
		// bootstrap (M1.11) always wires Catalog.
		return nil
	}
	var candidates []routing.ModelCandidate
	for _, m := range s.deps.Catalog.ListProvider("") {
		if !activeProviders[m.Provider] {
			continue
		}
		hasSub := providersWithSubscription[m.Provider] &&
			providers.SubscriptionAllowed(m.Provider, m.ModelID)

		// M3.4b — capability override. If the provider's executor
		// implements `executor.CapabilityOverrider`, run the catalog
		// flags through it; otherwise pass through unchanged. The
		// type-assertion short-circuits when (a) no executor is
		// registered for the provider, or (b) the executor doesn't
		// implement the interface (current state for all executors
		// until M3.5 wires implementations).
		caps := executor.Capabilities{
			SupportsImages:   m.Caps.SupportsImages,
			SupportsTools:    m.Caps.SupportsTools,
			SupportsThinking: m.Caps.SupportsThinking,
		}
		// M5.6 + AC-A6 (C2 fold): variant-keyed capability resolution.
		// Pick the variant for (provider, sample-conn's auth_type); when
		// no sample connection exists for this provider, fall back to
		// wildcard (provider, "") via Variants.Get's built-in fallback.
		if s.deps.Variants != nil {
			authType := sampleAuthType[m.Provider]
			if exec, ok := s.deps.Variants.Get(m.Provider, authType); ok {
				if overrider, ok := exec.(executor.CapabilityOverrider); ok {
					caps = overrider.OverrideCapabilities(m.ModelID, caps)
				}
			}
		}

		// M3.4b — cache-hit-rate enrichment. Two-way gate: only fire
		// the SQL when (1) the strategy actually consumes the result
		// (`effectivePrice` runs under StrategyCheap only) AND (2) the
		// model has non-zero CacheRead pricing (otherwise the cache-
		// aware blend collapses back to InputPrice — no value in the
		// query). When either gate is closed, CachedRatio stays at 0
		// — `effectivePrice` then degenerates to InputPrice and the
		// pre-M3.3 ordering is preserved (AC26b).
		//
		// The query is memoized by connID so K providers × N models
		// per provider produces at most K queries, not K×N (post-
		// review fix).
		cacheReadPrice := m.Pricing.CacheRead
		var cachedRatio float64
		if strategy == routing.StrategyCheap && cacheReadPrice > 0 {
			if connID, ok := sampleConn[m.Provider]; ok && connID != "" {
				entry, seen := hitRateByConn[connID]
				if !seen {
					// Errors are swallowed deliberately — a transient SQL
					// failure shouldn't disqualify the model from routing.
					// We cache the zero-result so we don't re-query for the
					// same connID later in the same request.
					if r, err := s.deps.Store.GetCacheHitRate(ctx, connID, 24*time.Hour); err == nil {
						entry = cacheRatioCache{ratio: r, cached: true}
					} else {
						entry = cacheRatioCache{ratio: 0, cached: true}
					}
					hitRateByConn[connID] = entry
				}
				cachedRatio = entry.ratio
			}
		}

		candidates = append(candidates, routing.ModelCandidate{
			Provider:                  m.Provider,
			Model:                     m.ModelID,
			Tier:                      m.Tier,
			InputPrice:                m.Pricing.Input,
			ContextWindow:             m.ContextWindow,
			SupportsImages:            caps.SupportsImages,
			SupportsTools:             caps.SupportsTools,
			SupportsThinking:          caps.SupportsThinking,
			HasSubscriptionConnection: hasSub,
			CacheReadPrice:            cacheReadPrice,
			CachedRatio:               cachedRatio,
		})
	}
	return candidates
}

// selectConnection picks a connection and marks it Active. Returns retryAfterSec > 0
// when all connections are rate-limited. strategy chooses the candidate
// ordering (SelectDefault for every non-auto:p2c/reset-aware request).
func (s *Server) selectConnection(providerID, model string, excludeIDs []string, strategy provider.SelectStrategy) (*ConnectionInfo, int, error) {
	// Cycle 20260517-provider-auth-variants M5.5 (Q9 + m4 fold):
	// auto_detect recovery branch REMOVED — auth_type=auto_detect rows
	// are converted to auth_type=subscription at create time
	// (routes_api.go:249-251), so the runtime auto_detect surface is
	// dead code per memory `b51e9198`. The helpers
	// recoverAutoDetectConnections + resolveAutoDetectCredentials are
	// likewise deleted below.

	result, err := s.deps.ProviderSelector.Select(providerID, model, excludeIDs, strategy)
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
		// Already grabbed by another goroutine, or concurrently disabled.
		// Select may have claimed this connection's HALF_OPEN trial slot —
		// release it before abandoning the connection, or the slot strands
		// (the deferred ReleaseHalfOpenTrial in executeRequest only covers
		// the connection it is handed, not one dropped here). Idempotent and
		// a no-op for a CLOSED connection that never claimed a slot — spec §4.
		conn.ReleaseHalfOpenTrial()
		if excludeIDs == nil {
			excludeIDs = []string{}
		}
		return s.selectConnection(providerID, model, append(excludeIDs, conn.ID), strategy)
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

	// Cycle 20260517-provider-auth-variants M2.6.1 + M5.3: the
	// ExchangedToken-empty preflight branch was REMOVED. Variant
	// dispatch now routes (openai, subscription) to
	// CodexSubscriptionExecutor which:
	//   - uses Credentials.AccessToken (the PKCE access_token) directly
	//   - implements PreflightChecker — empty AccessToken → ErrTierMissingScopes
	// The variant's preflight is invoked by routes_v1.go via
	// `preflightCreds(variantExec, creds)` (the helper added at M1.6).
	// Pre-rip behavior was tied to api.openai.com/v1/responses (wrong-path
	// per memory `f32bbc73`).

	// For subscription connections, also pull through AuthStore so we
	// pick up provider_data → AccountID → ExtraHeaders (specifically
	// the ChatGPT-Account-ID header DefaultExecutor injects when
	// serving requests via an OpenAI subscription). Non-fatal if the
	// AuthStore isn't wired or the credential parse fails — the
	// executor still has the access token and will work for everything
	// that doesn't need extra headers.
	if storedConn.AuthType == auth.AuthTypeSubscription && s.deps.AuthStore != nil {
		if cred, err := s.deps.AuthStore.GetCredential(conn.ID); err == nil && cred != nil {
			creds.ExtraHeaders = cred.ExtraHeaders()
		}
	}

	// Cycle 20260517-provider-auth-variants M5.5 (Q9 + m4 fold):
	// auto_detect request-time filesystem-read branch REMOVED. The
	// dashboard's auto-detect flow converts auth_type=auto_detect to
	// auth_type=subscription at create time (routes_api.go:249-251) and
	// stores the tokens encrypted in DB; no live filesystem read needed
	// at request time. Per memory `b51e9198`, the request-time branch
	// was dead code for dashboard-created connections.

	return &ConnectionInfo{
		ID:          conn.ID,
		Credentials: creds,
	}, 0, nil
}

// applyQuotaWindow parses an upstream response's rate-limit headers and stores
// the resulting best-effort QuotaWindow on the connection (M3, spec §3). It is
// invoked once per response that carries headers, from executeRequest's two
// dispositions — the success return and the 4xx/5xx branch. Best-effort: a
// connection no longer registered, or a response with no recognized rate-limit
// header, simply yields Known=false — it never blocks selection or errors.
func (s *Server) applyQuotaWindow(connID, providerID string, h http.Header) {
	pc := s.deps.ProviderSelector.ConnectionByID(connID)
	if pc == nil {
		return
	}
	now := time.Now()
	info := executor.ParseRateLimitReset(providerID, h, now)
	qw := provider.QuotaWindow{
		Remaining: info.Remaining,
		Known:     info.Known || info.RemainingKnown,
	}
	if info.Known && info.Reset > 0 {
		qw.ResetAt = now.Add(info.Reset)
	}
	pc.SetQuotaWindow(qw)
}

// markConnectionResult transitions the connection state based on the upstream
// outcome — the M2 circuit-breaker classifier (spec §3.2). A transport error
// and a 5xx open the breaker as FailureTransient; a 429 opens it as
// FailureRateLimit, honoring retryAfter (the time until the rate limit resets,
// parsed from the response's rate-limit headers by the caller; 0 when no header
// was present). The 401/403 and 2xx branches are unchanged by M2 — auth
// failures drive the Auth facet, not the breaker.
//
// For 401/403, also invalidates the in-memory credential cache so the next
// request triggers a refresh attempt rather than reusing a token the upstream
// just rejected. The respBody is scanned for model-level rejection markers
// (distinct from auth-token rejection) — when present, the model is added to
// the connection's per-model denylist (1h TTL) so the selector skips it for
// future requests on this connection.
func (s *Server) markConnectionResult(connID, model string, statusCode int, respBody []byte, err error, retryAfter time.Duration) {
	conn := s.deps.ProviderSelector.ConnectionByID(connID)
	if conn == nil {
		return
	}

	// The facet transition methods return errors only when the transition is
	// rejected (e.g., already in AuthExpired due to a concurrent failure, or
	// OpenBreaker re-opening an already-OPEN breaker).
	// We log these at debug-as-warn level so operators can see stuck-state
	// situations without inundating logs in the common case. Cred
	// invalidation runs regardless: even if the state transition couldn't
	// happen, the in-memory token is known-bad and dropping it is correct.
	var terr error
	switch {
	case err != nil:
		// Transport/network error — classified transient (§3.2). SetLastError
		// preserves the dashboard message; OpenBreaker records none of its own.
		conn.SetLastError(err)
		terr = conn.OpenBreaker(provider.FailureTransient, 0, model)
	case statusCode == 429:
		// Upstream rate limit (§3.2). retryAfter is honored by CooldownFor;
		// OpenBreaker escalates the backoff level internally.
		terr = conn.OpenBreaker(provider.FailureRateLimit, retryAfter, model)
	case statusCode == 401 || statusCode == 403:
		// Differentiate "this token can't do this model" from "this
		// token is dead". Model-tier rejections only denylist the
		// model on this connection — the rest of the connection's
		// model surface remains usable, no refresh needed. Auth
		// failures still hit AuthExpired + credential invalidation.
		if statusCode == 403 && model != "" && isModelRejection(respBody) {
			conn.RecordModelRejection(model)
			// Treat as a successful response from the connection's
			// perspective — the upstream did answer, just declined
			// this model. Keeps the connection's backoff/error state
			// clean.
			terr = conn.MarkSuccess()
		} else {
			// Cycle 20260517-provider-auth-variants M5.4: variant-aware
			// tier-error discrimination. The variant's ParseAuthError
			// inspects the response body and returns a typed error (e.g.,
			// ErrTierMissingScopes) when it matches a known pattern.
			// Replaces the inline `bytes.Contains(respBody, "Missing scopes")`
			// sniff that hardcoded the openai+subscription error string.
			if s.deps.Variants != nil {
				connInfo := s.deps.ProviderSelector.ConnectionByID(connID)
				if connInfo != nil {
					if variantExec, ok := s.deps.Variants.Get(connInfo.Provider, connInfo.AuthType); ok {
						if friendlyErr := parseAuthError(variantExec, statusCode, respBody); friendlyErr != nil {
							conn.SetLastError(friendlyErr)
						}
					}
				}
			}
			terr = conn.MarkAuthExpired()
			conn.InvalidateCredential()
		}
	case statusCode >= 500:
		// Upstream 5xx — classified transient (§3.2). SetLastError preserves the
		// dashboard message; CooldownFor ignores retryAfter for the transient
		// kind, but it is passed for consistency with the 429 branch.
		conn.SetLastError(fmt.Errorf("upstream %d", statusCode))
		terr = conn.OpenBreaker(provider.FailureTransient, retryAfter, model)
	default:
		terr = conn.MarkSuccess()
	}

	if terr != nil {
		slog.Warn("connection state transition rejected",
			"conn_id", connID,
			"model", model,
			"status", statusCode,
			"err", terr,
		)
	}
}

// modelRejectionPattern matches upstream error bodies indicating the model
// is unavailable for the connection's auth context (e.g., a subscription
// token that lacks access to a particular model tier). Distinct from a
// generic auth rejection — those keep the model in the routing pool for
// other connections. Compiled once at package init.
var modelRejectionPattern = regexp.MustCompile(`(?i)model.{0,40}(not available|unsupported|unavailable|forbidden|access denied|not (?:supported|allowed)|invalid)`)

// isModelRejection reports whether the upstream's response body suggests
// the connection's auth context cannot serve this model (vs. a generic
// auth failure that affects all models on the connection).
func isModelRejection(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// Truncate the regex search to a sane prefix — error bodies can be huge
	// for streaming responses.
	const maxScan = 4 * 1024
	if len(body) > maxScan {
		body = body[:maxScan]
	}
	return modelRejectionPattern.Match(body)
}

func (s *Server) trackUsage(requestID, provider, model, connectionID, apiKeyID string, u *canonical.Usage, startTime time.Time, status string, tokensBefore int) {
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

	var cacheReadTokens, cacheWriteTokens int
	if u != nil {
		cacheReadTokens = u.CacheReadTokens
		cacheWriteTokens = u.CacheCreationTokens
	}

	// Determine cost_source from the connection's AuthType. Subscription
	// connections record cost=0 (user paid flat subscription); the
	// dashboard computes "savings" against the would-have-been API cost.
	//
	// Models Discovery M1.9a — cost computed via catalog.Registry's
	// EstimateCost instead of the static config helper. The
	// TokenBreakdown carries cache columns so cache-aware pricing
	// applies whenever the catalog row has non-zero
	// CacheRead/CacheWrite (Anthropic Input*1.25 etc.).
	costSource := "apikey"
	var cost float64
	if s.deps.Catalog != nil {
		cost = s.deps.Catalog.EstimateCost(provider, model, catalog.TokenBreakdown{
			Input:        inputTokens,
			Output:       outputTokens,
			CacheReadIn:  cacheReadTokens,
			CacheWriteIn: cacheWriteTokens,
		})
	}
	if conn := s.deps.ProviderSelector.ConnectionByID(connectionID); conn != nil {
		if conn.AuthType == "subscription" {
			costSource = "subscription"
			cost = 0
		}
	}

	// tokens_after — the provider's actual input-token count of the
	// (compressed) request. Recorded only when compression ran (tokensBefore
	// > 0), so an uncompressed request leaves both measurement terms 0 — the
	// dual-sourced savings figure (M4 §6).
	tokensAfter := 0
	if tokensBefore > 0 {
		tokensAfter = inputTokens
	}

	// M4 §6 — per-request structured savings log, each term honestly
	// sourced (tokenizer estimate vs. provider-actual). Emitted only when
	// compression actually ran.
	if tokensBefore > 0 {
		slog.Info("compression savings",
			"request_id", requestID,
			"tokens_before", tokensBefore, "tokens_before_source", "tokenizer",
			"tokens_after", tokensAfter, "tokens_after_source", "provider",
		)
	}

	entry := &usage.Entry{
		RequestID:        requestID,
		Provider:         provider,
		Model:            model,
		ConnectionID:     connectionID,
		APIKeyID:         apiKeyID,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		TotalTokens:      totalTokens,
		CacheReadTokens:  cacheReadTokens,
		CacheWriteTokens: cacheWriteTokens,
		TokensBefore:     tokensBefore,
		TokensAfter:      tokensAfter,
		Cost:             cost,
		CostSource:       costSource,
		Latency:          time.Since(startTime),
		Status:           status,
		CreatedAt:        time.Now(),
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
			var blocks []struct {
				Text string `json:"text"`
			}
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
	if from == canonical.FormatResponses && to == canonical.FormatOpenAI {
		return responsesResponseToOpenAI(body, model)
	}
	return body, nil
}

// responsesResponseToOpenAI translates a /v1/responses upstream non-streaming
// body into OpenAI chat-completions shape so the client (e.g., Continue) sees
// the familiar `choices[].message.content` envelope. Uses
// openairesp.ParseUpstreamResponse for assistant-text + usage extraction
// (the translator owns the wire-shape knowledge); this function just
// re-serializes into the chat-completions wrapper.
//
// If ParseUpstreamResponse returns openairesp.ErrTierMissingScopes, we
// re-emit the body as a chat-completions error with the friendly message
// so clients display something actionable.
func responsesResponseToOpenAI(body []byte, model string) ([]byte, error) {
	text, usage, err := openairesp.ParseUpstreamResponse(body)
	if err != nil {
		// Surface tier-error in chat-completions-shaped error envelope.
		// Clients that recognise OpenAI's error shape (Continue, openai-python)
		// will render `error.message` directly.
		msg := err.Error()
		if errors.Is(err, openairesp.ErrTierMissingScopes) {
			msg = openairesp.ErrTierMissingScopes.Error()
		}
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "invalid_request_error",
				"code":    "openai_subscription_responses",
			},
		})
	}
	resp := map[string]any{
		"id":     "chatcmpl-from-responses",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
	}
	if usage != nil {
		resp["usage"] = map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	return json.Marshal(resp)
}

// Cycle 20260517-provider-auth-variants M5.2:
// resolveTargetFormat + resolveTargetFormatByConnID helpers DELETED.
// Their hardcoded openai+subscription → FormatResponses logic moved
// into CodexSubscriptionExecutor.Format(); callers now invoke
// formatOf(variantExec, providerID) at the two former call sites
// (executeRequest L244 + forwardResult L539).

// ---- Variant optional-interface helpers (M1.6 of cycle 20260517-provider-auth-variants) ----
//
// These free functions are the single-point queries the route handler uses
// to fetch per-variant behavior from a chosen variant Executor. Type-assert
// against the optional interfaces declared in internal/executor/executor.go
// (Formatted, OAuthIdentified, AuthErrorParser, PreflightChecker); fall
// through to safe defaults when the variant doesn't implement them.
//
// memory `fb0b4ef62` rule: RetryExecutor wrapper declares each forwarder
// method, so type-assertions on a wrapped executor always succeed — the
// forwarders return safe defaults if the inner doesn't opt in.
//
// Implementing variants today:
//   - Formatted        : CodexSubscriptionExecutor (M2) + ClaudeMaxExecutor (M3) + future variants.
//   - OAuthIdentified  : ClaudeMaxExecutor (M3) only — claude.ai OAuth tokens are
//                        scoped for Claude Code and the translator must identify accordingly.
//   - AuthErrorParser  : CodexSubscriptionExecutor (M2) + maybe ClaudeMaxExecutor (M3 live).
//   - PreflightChecker : CodexSubscriptionExecutor (M2) — empty access_token short-circuits.

// formatOf returns the variant's target wire format if it implements
// Formatted; otherwise falls back to the provider's natural target format
// via translate.DetectTargetFormat. Replaces resolveTargetFormat /
// resolveTargetFormatByConnID at M5.2.
func formatOf(e executor.Executor, providerID string) canonical.Format {
	if f, ok := e.(executor.Formatted); ok {
		if got := f.Format(); got != "" {
			return got
		}
	}
	return translate.DetectTargetFormat(providerID)
}

// needsOAuthIdentity returns whether the chosen variant's translator must
// prepend an OAuth-identity system block (currently only claude+subscription
// via ClaudeMaxExecutor at M3).
func needsOAuthIdentity(e executor.Executor) bool {
	if o, ok := e.(executor.OAuthIdentified); ok {
		return o.NeedsOAuthIdentity()
	}
	return false
}

// parseAuthError lets the variant translate provider-specific auth-error
// response bodies into typed Go errors (e.g., ErrTierMissingScopes). Replaces
// the inline `bytes.Contains(respBody, ...)` sniff at routes_v1.go:1452-1454
// when M5.4 lifts it.
func parseAuthError(e executor.Executor, statusCode int, body []byte) error {
	if p, ok := e.(executor.AuthErrorParser); ok {
		return p.ParseAuthError(statusCode, body)
	}
	return nil
}

// preflightCreds lets the variant reject a connection that lacks viable
// credentials BEFORE the upstream HTTP call. Replaces the inline preflight
// at routes_v1.go:1314-1334 when M5.3 lifts it.
func preflightCreds(e executor.Executor, creds *executor.Credentials) error {
	if p, ok := e.(executor.PreflightChecker); ok {
		return p.PreflightCredentials(creds)
	}
	return nil
}

// translateOptsFor builds the TranslateOpts the variant Executor needs.
// MUST be called at EVERY FromCanonical / TranslateRequest call site so
// variant-dependent flags (currently EmitOAuthIdentity for ClaudeMax)
// survive re-serialization in the cache-hint + context-bridge paths.
//
// Review-fold C1 of the holistic /review at 2026-05-17: without this
// helper, cache-hint or bridge re-serialization silently drops the
// OAuth-identity block from anthropic+subscription requests because the
// inline `translate.TranslateOpts{Model, Provider, Stream}` literals at
// those sites didn't carry the flag.
func translateOptsFor(e executor.Executor, model, providerID string, stream bool) translate.TranslateOpts {
	return translate.TranslateOpts{
		Model:             model,
		Provider:          providerID,
		Stream:            stream,
		EmitOAuthIdentity: needsOAuthIdentity(e),
	}
}

// resolveVariantExec picks the variant Executor for the chosen connection.
// Returns nil if (a) Variants registry isn't wired (test fixtures pre-M1),
// or (b) the variant isn't registered for (providerID, conn.AuthType).
//
// The route handler uses the returned executor ONLY to query optional
// interfaces (Format, NeedsOAuthIdentity, ParseAuthError, PreflightChecker)
// via the helpers above. The actual upstream Execute call still goes through
// s.deps.Executors[providerID] in M1; M5.6 migrates the dispatch site to
// also use Variants.Get.
//
// Logs the resolution at slog.Debug level for observability (m-R2
// review-fold) — gives ops a way to see which variant ran each request
// without adding a per-request structured field.
func (s *Server) resolveVariantExec(providerID string, conn *ConnectionInfo) executor.Executor {
	if s.deps.Variants == nil {
		return nil
	}
	authType := ""
	if conn != nil && conn.Credentials != nil {
		authType = conn.Credentials.AuthType
	}
	exec, ok := s.deps.Variants.Get(providerID, authType)
	if !ok {
		return nil
	}
	slog.Debug("variant resolved",
		"provider", providerID,
		"auth_type", authType,
		"executor", fmt.Sprintf("%T", exec))
	return exec
}

func claudeResponseToOpenAI(body []byte, model string) ([]byte, error) {
	var claude struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
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
