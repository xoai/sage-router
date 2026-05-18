package translate

import (
	"fmt"
	"sage-router/pkg/canonical"
	"sync"
)

// Translator converts between a provider's wire format and canonical.
type Translator interface {
	Format() canonical.Format
	DetectInbound(endpoint string, body []byte) bool
	ToCanonical(body []byte, opts TranslateOpts) (*canonical.Request, error)
	FromCanonical(req *canonical.Request, opts TranslateOpts) ([]byte, error)
	StreamChunkToCanonical(data []byte, state *StreamState) ([]canonical.Chunk, error)
	CanonicalToStreamChunk(chunk canonical.Chunk, state *StreamState) ([]byte, error)
}

// TranslateOpts carries context needed during translation.
type TranslateOpts struct {
	Model       string
	Provider    string
	Stream      bool
	Credentials any

	// EmitOAuthIdentity instructs the target translator to prepend an
	// OAuth-identity system block when building the request body. Currently
	// honored by the claude translator (M3 of cycle 20260517-provider-auth-variants:
	// prepend CLAUDE_CODE_IDENTITY when serving anthropic+subscription
	// requests). Other translators ignore the flag.
	//
	// Set by routes_v1.go::translateOptsFor based on the chosen variant
	// Executor's NeedsOAuthIdentity() return value. Wiring landed in M1.6b;
	// the flag is a no-op until M3 wires consumption in the claude translator.
	EmitOAuthIdentity bool
}

// StreamState carries mutable state across streaming chunks.
type StreamState struct {
	MessageID    string
	Model        string
	BlockIndex   int
	InThinking   bool
	InToolCall   bool
	ToolCalls    map[string]*ToolCallAccumulator
	FinishReason string
	Usage        *canonical.Usage
	Custom       map[string]any
}

// NewStreamState creates an initialized StreamState.
func NewStreamState() *StreamState {
	return &StreamState{
		ToolCalls: make(map[string]*ToolCallAccumulator),
		Custom:    make(map[string]any),
	}
}

// ToolCallAccumulator collects incremental tool call data.
type ToolCallAccumulator struct {
	ID        string
	Name      string
	Arguments string
}

// Registry holds all registered translators.
type Registry struct {
	mu       sync.RWMutex
	byFormat map[canonical.Format]Translator
}

// NewRegistry creates an empty translator registry.
func NewRegistry() *Registry {
	return &Registry{
		byFormat: make(map[canonical.Format]Translator),
	}
}

// Register adds a translator to the registry.
func (r *Registry) Register(t Translator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byFormat[t.Format()] = t
}

// Get returns the translator for the given format.
func (r *Registry) Get(format canonical.Format) (Translator, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byFormat[format]
	return t, ok
}

// DetectFormat tries each registered translator to detect the source format.
func (r *Registry) DetectFormat(endpoint string, body []byte) canonical.Format {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.byFormat {
		if t.DetectInbound(endpoint, body) {
			return t.Format()
		}
	}
	return canonical.FormatOpenAI // default
}

// TranslateRequest performs source → canonical → target translation.
//
// Model substitution contract (20260515-chat-routing-fix):
// When opts.Model is non-empty, it is the AUTHORITATIVE model name for
// the upstream-bound request — it overrides whatever model the inbound
// body carried (which may be a combo alias like "fast-fallback" or a
// namespaced ID like "anthropic/claude-haiku-4-5-20251001"). Callers
// in routes_v1.go resolve the bare model via resolveModel() before
// reaching here, and pass it in opts.Model. The substitution happens
// AFTER ToCanonical (which copies the inbound body's model into the
// canonical request) and BEFORE any FromCanonical / re-serialization
// path, so the bare resolved model lands in targetBody for every
// downstream consumer.
//
// Same-format note: source==target requests (e.g., OpenAI inbound
// targeting an OpenAI-compatible provider) used to pass body through
// verbatim. That optimization masked Bug A — the inbound body's
// model field reached upstream unchanged. The fix re-serializes via
// the source's own FromCanonical so opts.Model substitution is
// applied uniformly. The translators round-trip canonically; field
// ordering may differ vs. inbound but the canonical content is
// preserved (R3 in the spec / verified by translator tests).
func (r *Registry) TranslateRequest(
	source, target canonical.Format,
	body []byte,
	opts TranslateOpts,
) (*canonical.Request, []byte, error) {
	// Source → Canonical
	srcTranslator, ok := r.Get(source)
	if !ok {
		return nil, nil, fmt.Errorf("no translator for source format %q", source)
	}
	req, err := srcTranslator.ToCanonical(body, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("source→canonical: %w", err)
	}

	// Model substitution — AC-A1. opts.Model is authoritative when set;
	// empty opts.Model preserves the canonical's model from ToCanonical
	// (AC-A3, for callers that don't pre-resolve the model).
	if opts.Model != "" {
		req.Model = opts.Model
	}

	// Canonical → Target. Always serialize via the target translator
	// so the model substitution above lands in targetBody — including
	// the source==target case (AC-A2). The old pass-through optimization
	// (registry.go pre-20260515: `if target == source { targetBody = body }`)
	// was load-bearing for Bug A on Claude→Claude and OpenAI→OpenAI.
	tgtTranslator, ok := r.Get(target)
	if !ok {
		return nil, nil, fmt.Errorf("no translator for target format %q", target)
	}
	targetBody, err := tgtTranslator.FromCanonical(req, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("canonical→target: %w", err)
	}

	return req, targetBody, nil
}
