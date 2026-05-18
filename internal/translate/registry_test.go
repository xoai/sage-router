package translate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
	openaiTranslate "sage-router/internal/translate/openai"
	"sage-router/pkg/canonical"
)

// newTestRegistry registers the OpenAI and Claude translators for
// TranslateRequest tests in this cycle (20260515-chat-routing-fix).
// Gemini is not registered here because its translator is exercised
// separately and the bugs under test (model substitution, source==target
// re-serialization) are independent of which translators are registered.
func newTestRegistry() *translate.Registry {
	r := translate.NewRegistry()
	r.Register(openaiTranslate.New())
	r.Register(claudeTranslate.New())
	return r
}

// TestTranslateRequest_OptsModelAuthority_CrossFormat — AC-A1
// (cross-format). When source != target and opts.Model is set, the
// upstream-bound body's model field MUST equal opts.Model, NOT the
// inbound body's model. This is the canonical fix for Bug A in
// 20260515-chat-routing-fix: source translators copy the inbound
// body's `model` into the canonical request; without opts.Model
// authority at the registry layer, a combo alias like "fast-fallback"
// or a namespaced ID like "anthropic/..." leaks into the upstream
// request body.
func TestTranslateRequest_OptsModelAuthority_CrossFormat(t *testing.T) {
	r := newTestRegistry()
	body := []byte(`{"model":"fast-fallback","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.TranslateOpts{Model: "claude-haiku-4-5-20251001", Provider: "anthropic"}

	canonReq, targetBody, err := r.TranslateRequest(canonical.FormatOpenAI, canonical.FormatClaude, body, opts)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if canonReq.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("canonReq.Model = %q, want %q", canonReq.Model, "claude-haiku-4-5-20251001")
	}
	if !strings.Contains(string(targetBody), `"model":"claude-haiku-4-5-20251001"`) {
		t.Errorf("targetBody missing resolved bare model. body:\n%s", string(targetBody))
	}
	if strings.Contains(string(targetBody), "fast-fallback") {
		t.Errorf("targetBody contains inbound alias 'fast-fallback' — substitution failed. body:\n%s", string(targetBody))
	}
}

// TestTranslateRequest_OptsModelAuthority_SameFormat — AC-A2.
// When source == target (e.g., OpenAI → OpenAI), the function must
// NOT pass the body through verbatim. It must re-serialize via the
// source's own FromCanonical so opts.Model substitution lands in
// targetBody. The old pass-through optimization at registry.go:117
// masked Bug A for same-format requests (Claude→Claude, OpenAI→OpenAI).
func TestTranslateRequest_OptsModelAuthority_SameFormat(t *testing.T) {
	r := newTestRegistry()
	body := []byte(`{"model":"openai/gpt-4.1-nano","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.TranslateOpts{Model: "gpt-4.1-nano", Provider: "openai"}

	_, targetBody, err := r.TranslateRequest(canonical.FormatOpenAI, canonical.FormatOpenAI, body, opts)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if !strings.Contains(string(targetBody), `"model":"gpt-4.1-nano"`) {
		t.Errorf("targetBody missing bare model. body:\n%s", string(targetBody))
	}
	if strings.Contains(string(targetBody), "openai/") {
		t.Errorf("targetBody still has provider namespace prefix. body:\n%s", string(targetBody))
	}
}

// TestTranslateRequest_OptsModelAuthority_ClaudePassthrough — AC-A2
// for Claude→Claude. The user's curl repro for /v1/messages
// {"model":"anthropic/claude-haiku-4-5-20251001"} returned a 404
// with the namespaced model echoed back, because the pass-through
// branch preserved the inbound body verbatim. After fix, the bare
// "claude-haiku-4-5-20251001" must reach Anthropic.
func TestTranslateRequest_OptsModelAuthority_ClaudePassthrough(t *testing.T) {
	r := newTestRegistry()
	body := []byte(`{"model":"anthropic/claude-haiku-4-5-20251001","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.TranslateOpts{Model: "claude-haiku-4-5-20251001", Provider: "anthropic"}

	_, targetBody, err := r.TranslateRequest(canonical.FormatClaude, canonical.FormatClaude, body, opts)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if !strings.Contains(string(targetBody), `"model":"claude-haiku-4-5-20251001"`) {
		t.Errorf("targetBody missing bare model. body:\n%s", string(targetBody))
	}
	if strings.Contains(string(targetBody), "anthropic/") {
		t.Errorf("targetBody still has provider namespace prefix. body:\n%s", string(targetBody))
	}
}

// TestTranslateRequest_OptsModelAuthority_StreamingPath — AC-A7.
// The fix at registry.TranslateRequest applies opts.Model uniformly
// regardless of whether the request is streaming. There is no
// streaming-specific bypass — the same `req.Model = opts.Model`
// substitution + always-FromCanonical path runs for stream=true
// requests. This test explicitly pins that contract so a future
// refactor that splits the streaming path off cannot silently
// regress Bug A for streaming clients (the user's Continue
// configuration is streaming-by-default).
func TestTranslateRequest_OptsModelAuthority_StreamingPath(t *testing.T) {
	r := newTestRegistry()
	body := []byte(`{"model":"fast-fallback","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	opts := translate.TranslateOpts{Model: "claude-haiku-4-5-20251001", Provider: "anthropic", Stream: true}

	canonReq, targetBody, err := r.TranslateRequest(canonical.FormatOpenAI, canonical.FormatClaude, body, opts)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if canonReq.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("canonReq.Model = %q, want %q (streaming path)", canonReq.Model, "claude-haiku-4-5-20251001")
	}
	if !strings.Contains(string(targetBody), `"model":"claude-haiku-4-5-20251001"`) {
		t.Errorf("targetBody missing resolved bare model on streaming path. body:\n%s", string(targetBody))
	}
	if strings.Contains(string(targetBody), "fast-fallback") {
		t.Errorf("targetBody contains inbound alias on streaming path. body:\n%s", string(targetBody))
	}
	if !strings.Contains(string(targetBody), `"stream":true`) {
		t.Errorf("targetBody dropped stream flag. body:\n%s", string(targetBody))
	}
}

// TestTranslateRequest_EmptyOptsModel_BodyModelPreserved — AC-A3.
// When opts.Model is empty, the registry must not mutate the
// model. The inbound body's model field flows through unchanged.
// Today's single caller at routes_v1.go:227 always sets opts.Model,
// but the empty-opts contract must remain stable for future callers
// and to keep the registry layer's interface honest.
func TestTranslateRequest_EmptyOptsModel_BodyModelPreserved(t *testing.T) {
	r := newTestRegistry()
	body := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.TranslateOpts{Provider: "openai"} // Model intentionally empty

	canonReq, targetBody, err := r.TranslateRequest(canonical.FormatOpenAI, canonical.FormatOpenAI, body, opts)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if canonReq.Model != "gpt-4.1" {
		t.Errorf("canonReq.Model = %q, want %q (empty opts.Model should preserve body model)", canonReq.Model, "gpt-4.1")
	}
	if !strings.Contains(string(targetBody), `"model":"gpt-4.1"`) {
		t.Errorf("targetBody missing original model. body:\n%s", string(targetBody))
	}
}

// TestTranslateRequest_RegistryFixtures — AC-X5 golden-file regression.
// Walks testdata/registry/*.input.json, loads each + the paired
// *.expected.json, calls TranslateRequest with the input's source /
// target / opts / body, asserts the result is semantically equal to
// expected (jsonEqual tolerates field-ordering drift from canonical
// round-trip per R3). Adopts the same paired-file convention as
// existing fixtures under testdata/{openai,claude,gemini}/from_canonical/.
func TestTranslateRequest_RegistryFixtures(t *testing.T) {
	r := newTestRegistry()

	dir := filepath.Join("testdata", "registry")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read testdata/registry: %v", err)
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".input.json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".input.json")
		t.Run(name, func(t *testing.T) {
			inputPath := filepath.Join(dir, name+".input.json")
			expectedPath := filepath.Join(dir, name+".expected.json")

			inputData, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatalf("read %s: %v", inputPath, err)
			}
			var input struct {
				Source canonical.Format       `json:"source"`
				Target canonical.Format       `json:"target"`
				Opts   translate.TranslateOpts `json:"opts"`
				Body   json.RawMessage        `json:"body"`
			}
			if err := json.Unmarshal(inputData, &input); err != nil {
				t.Fatalf("unmarshal input: %v", err)
			}

			_, targetBody, err := r.TranslateRequest(input.Source, input.Target, input.Body, input.Opts)
			if err != nil {
				t.Fatalf("TranslateRequest: %v", err)
			}

			// Pretty-print targetBody so the golden file is human-readable.
			var pretty json.RawMessage
			out := targetBody
			if err := json.Unmarshal(targetBody, &pretty); err == nil {
				if p, err := json.MarshalIndent(pretty, "", "  "); err == nil {
					out = p
				}
			}

			if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
				if err := os.WriteFile(expectedPath, out, 0644); err != nil {
					t.Fatalf("write golden %s: %v", expectedPath, err)
				}
				t.Logf("wrote golden file %s", expectedPath)
				return
			}

			expected, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatalf("read %s: %v", expectedPath, err)
			}
			if !jsonEqual(targetBody, expected) {
				t.Errorf("targetBody mismatch for %s\n--- got ---\n%s\n--- expected ---\n%s",
					name, string(targetBody), string(expected))
			}
		})
	}
}
