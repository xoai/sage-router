package executor

import (
	"context"
	"io"
	"net/http"
	"time"

	"sage-router/pkg/canonical"
)

// Result holds the outcome of a single upstream API call.
type Result struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
	URL        string
	Latency    time.Duration
}

// Executor is the interface every provider-specific executor must implement.
type Executor interface {
	// Provider returns the canonical provider ID (e.g. "openai", "anthropic").
	Provider() string

	// Execute sends the request to the upstream provider and returns the raw
	// result. The caller is responsible for closing Result.Body.
	Execute(ctx context.Context, req *ExecuteRequest) (*Result, error)
}

// ExecuteRequest contains everything an Executor needs to build and send the
// upstream HTTP request.
type ExecuteRequest struct {
	Model       string
	Body        []byte
	Stream      bool
	Credentials *Credentials
	ProxyURL    string
	Endpoint    string // The target API endpoint path (e.g. "/v1/chat/completions")
}

// VariantKey identifies one (provider, auth_type) pair in the executor
// dispatch map. AuthType "" is the WILDCARD entry — matches any auth_type
// for that provider when no exact match is registered.
//
// See decision-executor-variant-abstraction.md for the dispatch rationale
// and decision-routing-single-point-dispatch.md for how the route handler
// queries variants. Cycle 20260517-provider-auth-variants M1.
type VariantKey struct {
	Provider string
	AuthType string
}

// Variants is the variant-keyed dispatch map. Build via NewVariants() and
// Register() per (provider, auth_type) pair; query via Get() at request
// time. Iterate() visits every registered pair (used by main.go's
// RetryExecutor wrap loop + startup-validation invariants).
//
// The internal map is unexported; the Iterate() API replaces an earlier
// All() that returned the map directly (m1 holistic-review fold —
// returning the map exposed it to accidental mutation post-init).
type Variants struct {
	m map[VariantKey]Executor
}

// NewVariants constructs an empty registry.
func NewVariants() *Variants {
	return &Variants{m: map[VariantKey]Executor{}}
}

// Register adds (or replaces) the executor for a (provider, auth_type) key.
// Called at boot from cmd/sage-router/main.go; not safe for concurrent
// post-init mutation (intentional — the registry is read-only after boot).
func (v *Variants) Register(k VariantKey, e Executor) {
	v.m[k] = e
}

// Get returns the executor for (provider, authType). Lookup is two-step:
//
//  1. Exact match on VariantKey{provider, authType}.
//  2. Wildcard fallback on VariantKey{provider, ""} — used by providers
//     that don't yet need variant splitting (gemini, github-copilot,
//     openrouter, ollama, default).
//
// Returns (nil, false) when neither is registered. The caller MUST handle
// the miss — there is no silent fallthrough to a default catch-all
// executor; that pattern was the source of memory `f32bbc73`'s wrong-path
// shipping in the predecessor cycle.
func (v *Variants) Get(provider, authType string) (Executor, bool) {
	if e, ok := v.m[VariantKey{Provider: provider, AuthType: authType}]; ok {
		return e, true
	}
	if e, ok := v.m[VariantKey{Provider: provider, AuthType: ""}]; ok {
		return e, true
	}
	return nil, false
}

// Iterate calls fn for each (key, executor) in the registry. Order is
// unspecified (Go map iteration). Used by main.go's RetryExecutor wrap
// loop and by startup-time invariant validation. Does NOT return the
// underlying map — callers can't mutate the registry through this API.
func (v *Variants) Iterate(fn func(VariantKey, Executor)) {
	for k, e := range v.m {
		fn(k, e)
	}
}

// ---- Optional interfaces (per decision-routing-single-point-dispatch.md) ----
//
// Variants opt in to per-variant behaviors by implementing these interfaces.
// The route handler queries them via type-assertion helpers (formatOf,
// needsOAuthIdentity, parseAuthError, preflightCreds) that fall through to
// safe defaults when not implemented. RetryExecutor wrappers declare the
// methods directly + delegate to the inner — see memory `fb0b4ef62` for
// why this discipline matters.

// Formatted variants declare their target wire format. Replaces the
// resolveTargetFormat / resolveTargetFormatByConnID helpers that hardcoded
// openai+subscription → FormatResponses in routes_v1.go.
type Formatted interface {
	Executor
	Format() canonical.Format
}

// OAuthIdentified variants whose translator must prepend an OAuth-identity
// system block (currently ClaudeMaxExecutor — claude.ai OAuth tokens are
// scoped for Claude Code, so requests identify as such). Used by
// routes_v1.go's translateOptsFor helper to plumb the flag through to
// translate.TranslateOpts.EmitOAuthIdentity.
type OAuthIdentified interface {
	Executor
	NeedsOAuthIdentity() bool
}

// AuthErrorParser variants that translate provider-specific authentication
// errors (4xx with a body shape) into typed Go errors. The route handler
// surfaces the typed error via connection.SetLastError for friendly
// dashboard display.
type AuthErrorParser interface {
	Executor
	ParseAuthError(statusCode int, body []byte) error
}

// PreflightChecker variants can synchronously reject a connection that
// lacks viable credentials BEFORE the upstream call fires. Saves a round
// trip when the variant knows up-front the request can't succeed.
type PreflightChecker interface {
	Executor
	PreflightCredentials(creds *Credentials) error
}

// Credentials carries authentication material for a single upstream connection.
type Credentials struct {
	ConnectionID string
	AuthType     string // "apikey" | "subscription" | "none" (canonical — see auth.NormalizeAuthType)
	AccessToken  string
	RefreshToken string
	// ExchangedToken removed in cycle 20260517-provider-auth-variants M2.6.3.
	// The variant abstraction routes (openai, subscription) to
	// CodexSubscriptionExecutor which uses AccessToken directly.
	APIKey string
	ExpiresAt      time.Time
	ProviderData   map[string]any
	// ExtraHeaders are provider-specific request headers beyond the standard
	// Authorization header. The server populates this from auth.Credential.ExtraHeaders().
	// Currently used by OpenAI subscription for the ChatGPT-Account-ID header.
	ExtraHeaders map[string]string
}
