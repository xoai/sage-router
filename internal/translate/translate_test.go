package translate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
)

// goldenRequestTest loads an input JSON file, runs ToCanonical, and compares
// the result to the expected JSON file. If the expected file doesn't exist and
// the -update flag is set, it writes the actual output as the new golden file.
func goldenRequestTest(t *testing.T, tr translate.Translator, dir, name string, opts translate.TranslateOpts) {
	t.Helper()

	inputPath := filepath.Join("testdata", dir, "request", name+".input.json")
	expectedPath := filepath.Join("testdata", dir, "request", name+".expected.json")

	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read input %s: %v", inputPath, err)
	}

	got, err := tr.ToCanonical(input, opts)
	if err != nil {
		t.Fatalf("ToCanonical: %v", err)
	}

	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		// Write golden file on first run
		if err := os.WriteFile(expectedPath, gotJSON, 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden file %s", expectedPath)
		return
	}

	expected, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read expected %s: %v", expectedPath, err)
	}

	if !jsonEqual(gotJSON, expected) {
		t.Errorf("ToCanonical mismatch for %s\n--- got ---\n%s\n--- expected ---\n%s",
			name, string(gotJSON), string(expected))
	}
}

// goldenFromCanonicalTest loads a canonical JSON, runs FromCanonical, and
// compares to the expected provider-format JSON.
func goldenFromCanonicalTest(t *testing.T, tr translate.Translator, dir, name string, opts translate.TranslateOpts) {
	t.Helper()

	inputPath := filepath.Join("testdata", dir, "from_canonical", name+".input.json")
	expectedPath := filepath.Join("testdata", dir, "from_canonical", name+".expected.json")

	inputData, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read input %s: %v", inputPath, err)
	}

	var req canonical.Request
	if err := json.Unmarshal(inputData, &req); err != nil {
		t.Fatalf("unmarshal canonical request: %v", err)
	}

	got, err := tr.FromCanonical(&req, opts)
	if err != nil {
		t.Fatalf("FromCanonical: %v", err)
	}

	// Pretty-print the output
	var pretty json.RawMessage
	if err := json.Unmarshal(got, &pretty); err == nil {
		got, _ = json.MarshalIndent(pretty, "", "  ")
	}

	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		if err := os.WriteFile(expectedPath, got, 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden file %s", expectedPath)
		return
	}

	expected, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}

	if !jsonEqual(got, expected) {
		t.Errorf("FromCanonical mismatch for %s\n--- got ---\n%s\n--- expected ---\n%s",
			name, string(got), string(expected))
	}
}

// goldenStreamTest loads a series of SSE data lines, runs StreamChunkToCanonical
// for each, and compares the collected chunks to the expected JSON.
func goldenStreamTest(t *testing.T, tr translate.Translator, dir, name string) {
	t.Helper()

	inputPath := filepath.Join("testdata", dir, "stream", name+".input.jsonl")
	expectedPath := filepath.Join("testdata", dir, "stream", name+".expected.json")

	inputData, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read input %s: %v", inputPath, err)
	}

	state := translate.NewStreamState()
	var allChunks []canonical.Chunk

	lines := strings.Split(strings.TrimSpace(string(inputData)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "data: [DONE]" {
			continue
		}
		// Strip "data: " prefix if present
		line = strings.TrimPrefix(line, "data: ")

		chunks, err := tr.StreamChunkToCanonical([]byte(line), state)
		if err != nil {
			t.Logf("StreamChunkToCanonical error (may be expected): %v for line: %s", err, line)
			continue
		}
		allChunks = append(allChunks, chunks...)
	}

	gotJSON, err := json.MarshalIndent(allChunks, "", "  ")
	if err != nil {
		t.Fatalf("marshal chunks: %v", err)
	}

	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		if err := os.WriteFile(expectedPath, gotJSON, 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden file %s", expectedPath)
		return
	}

	expected, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}

	if !jsonEqual(gotJSON, expected) {
		t.Errorf("StreamChunkToCanonical mismatch for %s\n--- got ---\n%s\n--- expected ---\n%s",
			name, string(gotJSON), string(expected))
	}
}

// jsonEqual compares two JSON byte slices for semantic equality,
// ignoring whitespace differences.
func jsonEqual(a, b []byte) bool {
	var ja, jb any
	if err := json.Unmarshal(a, &ja); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &jb); err != nil {
		return false
	}
	na, _ := json.Marshal(ja)
	nb, _ := json.Marshal(jb)
	return string(na) == string(nb)
}
