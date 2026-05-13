package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Models Discovery M3.7 — token-leak regex set used by the
// subscription log-leak test extension AND mirrored by the
// `make grep-no-secrets` Makefile target. Any addition here must
// also update the Makefile's TOKEN_REGEX.
//
// Patterns cover all production-relevant provider credential shapes.
// The {20,} / {36,} quantifiers are tuned so the regexes match real
// credentials but NOT short test fixtures like "sk-test" or
// "AIza-stub" — the M3 cycle deliberately uses such non-matching
// fakes throughout the test suite.

// tokenLeakPattern carries one provider-credential regex plus the
// label used in error reporting. The regex object is reused across
// every test that scans output for token shapes — compiling once
// avoids paying the regex-compile cost in every test invocation.
type tokenLeakPattern struct {
	name  string
	regex *regexp.Regexp
}

// tokenLeakPatterns is the canonical list of provider credential
// shapes the project must never log. Wire any new provider here when
// it adds a new credential format.
var tokenLeakPatterns = []tokenLeakPattern{
	{name: "openai-apikey", regex: regexp.MustCompile(`sk-[a-zA-Z0-9_\-]{20,}`)},
	{name: "anthropic-apikey", regex: regexp.MustCompile(`sk-ant-[a-zA-Z0-9_\-]{20,}`)},
	{name: "google-apikey", regex: regexp.MustCompile(`AIza[A-Za-z0-9\-_]{20,}`)},
	{name: "github-pat", regex: regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`)},
	{name: "github-oauth", regex: regexp.MustCompile(`gho_[A-Za-z0-9]{36,}`)},
	// `ya29\.` floor raised from `+` to `{40,}` after M3.7 review
	// (MAJOR-1). Real Google OAuth access tokens are 100+ chars;
	// the original `+` quantifier false-positived on benign log lines
	// like `request finished with ya29.foo at line 12`. 40 chars is
	// the minimum that comfortably exceeds any plausible identifier
	// while still trapping real credentials.
	{name: "google-oauth", regex: regexp.MustCompile(`ya29\.[A-Za-z0-9\-_]{40,}`)},
	{name: "supabase-service", regex: regexp.MustCompile(`sbp_[A-Za-z0-9]{36,}`)},
}

// TestTokenLeakPatterns_MatchKnownShapes — sanity-check the regex set
// against synthetic samples assembled at runtime. Doing the assembly
// at runtime (via concatenation + strings.Repeat) means no string
// literal in this file matches the regex, so `make grep-no-secrets`
// scanning test output won't false-positive on this file's source.
//
// Positive samples MUST be constructed, not literal — the moment a
// 20+ char "sk-..." literal lives in this file, the Makefile grep
// scanning `go test -v` output starts flagging this very test.
func TestTokenLeakPatterns_MatchKnownShapes(t *testing.T) {
	long := strings.Repeat("A", 40) // 40 chars — exceeds every {20,} / {36,} quantifier

	cases := []struct {
		pattern string
		input   string
		want    bool
	}{
		// Positives: synthetic samples that must match.
		{"openai-apikey", "sk" + "-" + long, true},
		{"anthropic-apikey", "sk" + "-ant-" + long, true},
		{"google-apikey", "AIza" + long, true},
		{"github-pat", "ghp" + "_" + long, true},
		{"github-oauth", "gho" + "_" + long, true},
		{"google-oauth", "ya29" + "." + long, true},
		{"supabase-service", "sbp" + "_" + long, true},

		// Negatives: short fakes used throughout the test suite must NOT match.
		{"openai-apikey", "sk-test", false},
		{"openai-apikey", "sk-stub", false},
		{"anthropic-apikey", "sk-ant-test", false},
		{"google-apikey", "AIza-stub", false},
		{"github-pat", "ghp_short", false},
		{"google-oauth", "ya29", false},
		// M3.7 review MAJOR-1 follow-up: `ya29.foo` and similar short
		// suffixes are common in benign log lines (e.g.,
		// "request finished with ya29.something at line 12"). The new
		// {40,} floor must reject these so `make grep-no-secrets`
		// doesn't false-positive on innocuous strings.
		{"google-oauth", "ya29.foo", false},
		{"google-oauth", "ya29.short-suffix", false},
		{"google-oauth", "ya29." + strings.Repeat("A", 39), false}, // 1 short of the {40,} floor
		{"supabase-service", "sbp_short", false},

		// Cross-matches: anthropic samples must NOT match the openai pattern
		// even though both start with "sk-". The openai pattern intentionally
		// matches "sk-ant-..." too (since "sk-ant-..." is a valid "sk-..." +
		// 20 chars), which is the correct behavior — both patterns will fire
		// and either failure is informative.
	}
	for _, c := range cases {
		// Look up the regex by name.
		var pat *tokenLeakPattern
		for i := range tokenLeakPatterns {
			if tokenLeakPatterns[i].name == c.pattern {
				pat = &tokenLeakPatterns[i]
				break
			}
		}
		if pat == nil {
			t.Fatalf("pattern %q not in tokenLeakPatterns", c.pattern)
		}
		got := pat.regex.MatchString(c.input)
		if got != c.want {
			// Do NOT log the input — even a synthetic positive sample
			// would surface in test output and trigger the Makefile grep
			// during the very test that's supposed to detect such leaks.
			t.Errorf("pattern %q: MatchString = %v, want %v (input redacted; length=%d)",
				c.pattern, got, c.want, len(c.input))
		}
	}
}

// TestTokenLeakPatterns_MakefileMirrorsGoRegex — M3.7 review MAJOR-2.
//
// The Go regex set (`tokenLeakPatterns`) and the Makefile's `grep-no-secrets`
// target each carry their own copy of the provider-token alternation. The
// whole defense-in-depth design depends on those copies staying aligned:
// if the Go test adds a Cloudflare pattern but the Makefile doesn't, the
// `make lint` surface silently misses real Cloudflare credentials.
//
// This test enforces alignment by behavior — not string equality, which
// would be too brittle (the Makefile elides `sk-ant-` since it's subsumed
// by the broader `sk-` pattern). For each Go pattern, we construct a
// synthetic positive sample that satisfies the Go regex's quantifiers, and
// assert the Makefile's alternation also matches it. A drift like "Go has
// pattern X but Makefile alternation doesn't catch X-shaped credentials"
// fails this test loudly.
//
// The Makefile alternation is extracted by reading the Makefile and
// scanning for `grep -qE '<PATTERN>'` inside the `grep-no-secrets` target.
// If the Makefile's regex syntax stops being RE2-compatible (e.g., it
// uses a feature only POSIX ERE has), this test would fail to compile
// the extracted pattern — that itself is a useful signal.
func TestTokenLeakPatterns_MakefileMirrorsGoRegex(t *testing.T) {
	// Locate the Makefile. From internal/server/ it's two directories up.
	makefilePath := filepath.Join("..", "..", "Makefile")
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read Makefile at %s: %v", makefilePath, err)
	}

	// Extract the regex from the `grep -qE 'PATTERN'` clause. The
	// Makefile's grep target uses single quotes around the alternation;
	// PATTERN itself never contains a single quote, so the [^']* class
	// captures it cleanly.
	extractor := regexp.MustCompile(`grep -qE '([^']+)'`)
	m := extractor.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("could not locate `grep -qE '...'` in Makefile — has the grep-no-secrets target moved or been removed?")
	}
	makefileRegex, err := regexp.Compile(m[1])
	if err != nil {
		// Don't echo the regex itself in the error; an attacker reading CI
		// output gets a usable scanner regex either way (it's public in
		// the Makefile), but this test should not be the one to advertise.
		t.Fatalf("compile Makefile regex: %v", err)
	}

	// Synthetic positive sample length must exceed every Go quantifier's
	// minimum. Current max is the `ya29.{40,}` floor — 60 chars leaves
	// headroom for any future regex bump.
	long := strings.Repeat("A", 60)

	cases := []struct {
		patternName string
		positive    string // synthetic sample the Go pattern matches
	}{
		{"openai-apikey", "sk" + "-" + long},
		{"anthropic-apikey", "sk" + "-ant-" + long},
		{"google-apikey", "AIza" + long},
		{"github-pat", "ghp" + "_" + long},
		{"github-oauth", "gho" + "_" + long},
		{"google-oauth", "ya29" + "." + long},
		{"supabase-service", "sbp" + "_" + long},
	}
	for _, c := range cases {
		// Find the Go pattern by name (sanity — every patternName here
		// MUST be in tokenLeakPatterns).
		var goPat *tokenLeakPattern
		for i := range tokenLeakPatterns {
			if tokenLeakPatterns[i].name == c.patternName {
				goPat = &tokenLeakPatterns[i]
				break
			}
		}
		if goPat == nil {
			t.Errorf("pattern %q listed in drift-test cases but not in tokenLeakPatterns — drift inside the Go file itself",
				c.patternName)
			continue
		}
		// Sanity: the Go pattern matches its own synthetic positive.
		// If this fails, the test case's synthetic input doesn't satisfy
		// the Go regex's quantifiers — fix the input length, not the regex.
		if !goPat.regex.MatchString(c.positive) {
			t.Errorf("internal: Go pattern %q does not match its own synthetic input (input length=%d)",
				c.patternName, len(c.positive))
			continue
		}
		// The drift assertion: the Makefile alternation MUST also match
		// the synthetic positive. If not, the Makefile is missing the
		// pattern (or has tighter quantifiers than the Go set).
		if !makefileRegex.MatchString(c.positive) {
			t.Errorf("Makefile regex MISSES %q-shaped credential — drift between Go tokenLeakPatterns and Makefile grep-no-secrets (input length=%d; matched bytes redacted)",
				c.patternName, len(c.positive))
		}
	}
}

// scanForLeakedTokens returns any tokenLeakPattern that matched the
// input. Returns nil when no pattern matches.
//
// Used by tests that capture log output and want to defend against
// the broad "real token shape leaked into logs" failure class — a
// superset of the synthetic-substring check the original M3-of-
// subscription-auth test uses. Callers should report match by
// pattern NAME and position, never by the matched bytes, so the
// failure message itself doesn't echo the secret to CI output.
func scanForLeakedTokens(s string) []tokenLeakPattern {
	var hits []tokenLeakPattern
	for _, p := range tokenLeakPatterns {
		if p.regex.MatchString(s) {
			hits = append(hits, p)
		}
	}
	return hits
}
