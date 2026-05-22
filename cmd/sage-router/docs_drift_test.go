package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAGENTSMD_TestCommandMatchesCI is the doc-drift guard (cycle
// 20260522-tier3-hygiene, AC4): the test command quoted in AGENTS.md must
// be the exact command CI runs (.github/workflows/ci.yml), so the
// onboarding doc cannot silently drift from the CI pipeline. It runs inside
// the ordinary `go test ./...` — already in CI — so no new gate or
// dependency is needed.
func TestAGENTSMD_TestCommandMatchesCI(t *testing.T) {
	root := repoRoot(t)
	const testCmd = "go test ./... -race -count=1"

	if agents := readRepoFile(t, root, "AGENTS.md"); !strings.Contains(agents, testCmd) {
		t.Errorf("AGENTS.md does not contain the canonical test command %q", testCmd)
	}
	ci := readRepoFile(t, root, filepath.Join(".github", "workflows", "ci.yml"))
	if !strings.Contains(ci, testCmd) {
		t.Errorf("ci.yml does not contain %q — AGENTS.md may have drifted from CI", testCmd)
	}
}

// repoRoot walks up from the test's working directory (go test runs each
// package in its own directory) to the first ancestor containing go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found — no go.mod in any ancestor directory")
		}
		dir = parent
	}
}

func readRepoFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
