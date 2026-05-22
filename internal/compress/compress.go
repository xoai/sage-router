package compress

import (
	"encoding/json"

	"sage-router/internal/compress/tokenizer"
	"sage-router/pkg/canonical"
)

// compressThreshold is the fraction of a model's context window above which
// the pre-flight gate fires — the OmniRoute analysis §4.1 70% trigger
// (ADR §1). With the o200k_base tokenizer's ~±20% error band for
// Claude/Gemini this is really a ~58–87% context-fill band; the consequence
// of the error is only a slightly-early or slightly-late opt-in pass.
const compressThreshold = 0.70

// Compressor holds the loaded tokenizer and filter catalog. Construct it via
// NewCompressor; a nil *Compressor is valid and inert — every method is a
// safe no-op. That is how the server runs when the subsystem fails to load.
type Compressor struct {
	tok *tokenizer.Tokenizer
	cat *Catalog
}

// Result reports the outcome of a Compress call.
type Result struct {
	// Compressed is true if any tool-result content was changed.
	Compressed bool
	// TokensBefore is the tokenizer estimate of the request BEFORE
	// compression — the (estimated) term of the dual-sourced savings figure.
	TokensBefore int
}

// NewCompressor loads the tokenizer and the filter catalog. On any load
// failure it returns an error; the caller (cmd/sage-router) logs it and
// wires a nil Compressor, disabling compression while the server runs
// normally — fail-closed.
func NewCompressor() (*Compressor, error) {
	tok, err := tokenizer.LoadTokenizer()
	if err != nil {
		return nil, err
	}
	cat, err := LoadCatalog()
	if err != nil {
		return nil, err
	}
	return &Compressor{tok: tok, cat: cat}, nil
}

// Compress shrinks the tool-result content of req in place when the request
// is large enough to be worth it — the M4 pipeline stage. It mutates ONLY
// canonical.Content blocks of type TypeToolResult; System blocks, plain
// text, and tool calls are never touched (the cache/compression disjointness
// invariant, ADR §0).
//
// A nil Compressor is inert: a zero Result, no mutation. contextWindow <= 0
// means "unknown" (no catalog entry for the target model) — the upper-bound
// gate is then skipped and compression still runs, so a catalog miss does
// not silently disable the feature.
func (c *Compressor) Compress(req *canonical.Request, contextWindow int) Result {
	if c == nil || req == nil {
		return Result{}
	}
	before := c.estimateTokens(req)
	// Pre-flight gate: only compress a request that is large relative to the
	// target model's context window.
	if contextWindow > 0 && float64(before) <= compressThreshold*float64(contextWindow) {
		return Result{Compressed: false, TokensBefore: before}
	}
	changed := false
	for mi := range req.Messages {
		for ci := range req.Messages[mi].Content {
			ct := &req.Messages[mi].Content[ci]
			if ct.Type != canonical.TypeToolResult {
				continue
			}
			// A tool-result that is itself a structured JSON document is
			// left untouched: the line-oriented filters (collapse-repeats,
			// truncate-safe) would corrupt it (ADR §"filter catalog" — no
			// structured-output corruption). The compression target is
			// unstructured log spam — test output, build logs, file dumps —
			// not structured payloads.
			if json.Valid([]byte(ct.Text)) {
				continue
			}
			if out := c.cat.Apply(ct.Text); out != ct.Text {
				ct.Text = out
				changed = true
			}
		}
	}
	return Result{Compressed: changed, TokensBefore: before}
}

// estimateTokens is the tokenizer count of the request's text content — the
// pre-flight gate input and the tokens_before measurement. It is a coarse
// estimate by design (ADR Gap D): System text, message text, and tool-call
// arguments — the bulk of a request's tokens.
func (c *Compressor) estimateTokens(req *canonical.Request) int {
	if c == nil || c.tok == nil || req == nil {
		return 0
	}
	total := 0
	for _, sb := range req.System {
		total += c.tok.Count(sb.Text)
	}
	for _, m := range req.Messages {
		for _, ct := range m.Content {
			total += c.tok.Count(ct.Text)
			total += c.tok.Count(ct.Arguments)
		}
	}
	return total
}
