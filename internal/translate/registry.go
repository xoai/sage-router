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
func (r *Registry) TranslateRequest(
	source, target canonical.Format,
	body []byte,
	opts TranslateOpts,
) (*canonical.Request, []byte, error) {
	// Source → Canonical
	var req *canonical.Request
	var err error

	srcTranslator, ok := r.Get(source)
	if !ok {
		return nil, nil, fmt.Errorf("no translator for source format %q", source)
	}
	req, err = srcTranslator.ToCanonical(body, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("source→canonical: %w", err)
	}

	// Canonical → Target
	var targetBody []byte
	if target == source {
		targetBody = body // pass through
	} else {
		tgtTranslator, ok := r.Get(target)
		if !ok {
			return nil, nil, fmt.Errorf("no translator for target format %q", target)
		}
		targetBody, err = tgtTranslator.FromCanonical(req, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("canonical→target: %w", err)
		}
	}

	return req, targetBody, nil
}
