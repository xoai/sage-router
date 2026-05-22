package tokenizer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// M4 T1 — the vendored o200k_base vocab loads into a rank table, fail-closed.

func TestLoadTokenizer(t *testing.T) {
	tk, err := LoadTokenizer()
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	if tk == nil {
		t.Fatal("LoadTokenizer returned a nil tokenizer with no error")
	}
	if len(tk.ranks) != expectedRankCount {
		t.Errorf("rank table size = %d, want %d", len(tk.ranks), expectedRankCount)
	}
}

func TestLoadTokenizer_KnownRanks(t *testing.T) {
	tk, err := LoadTokenizer()
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	// Published o200k_base ranks: base64 "IQ==" decodes to "!" (rank 0),
	// "Ig==" to '"' (rank 1) — the first two BPE entries. Asserting these
	// verifies the base64-decode + line parse, not the BPE merge (that is T2).
	for _, c := range []struct {
		tok  string
		rank int
	}{
		{"!", 0},
		{"\"", 1},
	} {
		if got, ok := tk.ranks[c.tok]; !ok || got != c.rank {
			t.Errorf("ranks[%q] = (%d, %v), want (%d, true)", c.tok, got, ok, c.rank)
		}
	}
}

func TestLoadTokenizer_ChecksumPinned(t *testing.T) {
	sum := sha256.Sum256(o200kData)
	if got := hex.EncodeToString(sum[:]); got != o200kSHA256 {
		t.Errorf("vendored o200k_base.tiktoken checksum drifted: got %s, want %s\n"+
			"a truncated or replaced vocab file must not pass silently", got, o200kSHA256)
	}
}

func TestParseRanks_Malformed(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"missing separator", "IQ=="},
		{"bad base64", "@@@ 0"},
		{"non-numeric rank", "IQ== notanumber"},
		{"empty table", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRanks([]byte(tc.data)); err == nil {
				t.Errorf("parseRanks(%q): expected a typed error, got nil", tc.data)
			}
		})
	}
}

// M4 T2 — the BPE Count. AC1's reference is the vendored rank table itself
// (an independent published authority): by the definition of BPE, a string
// that is a key in the o200k_base table is exactly one token. These tests
// verify the merge against the published vocab — they are not circular (the
// reference is the table, not this Count implementation).

func mustLoad(t *testing.T) *Tokenizer {
	t.Helper()
	tk, err := LoadTokenizer()
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}
	return tk
}

func TestCount_EmptyAndNil(t *testing.T) {
	if got := mustLoad(t).Count(""); got != 0 {
		t.Errorf("Count(\"\") = %d, want 0", got)
	}
	var nilTk *Tokenizer
	if got := nilTk.Count("anything"); got != 0 {
		t.Errorf("nil Tokenizer Count = %d, want 0 (must be a safe no-op)", got)
	}
}

func TestCount_SingleVocabEntryIsOneToken(t *testing.T) {
	tk := mustLoad(t)
	// Each of these is a common o200k_base entry. The test fatals if the
	// premise (it IS a single vocab token) is broken, so the assertion
	// genuinely tests the merge.
	for _, s := range []string{"hello", " world", "the", " the", " a", " function"} {
		if _, ok := tk.ranks[s]; !ok {
			t.Fatalf("test premise broken: %q is not a single o200k_base vocab entry", s)
		}
		if got := tk.Count(s); got != 1 {
			t.Errorf("Count(%q) = %d, want 1 (it is one vocab entry — BPE must merge it whole)", s, got)
		}
	}
}

func TestCount_SingleBytes(t *testing.T) {
	tk := mustLoad(t)
	for _, s := range []string{"!", "a", "Z", "0", "@", "~"} {
		if got := tk.Count(s); got != 1 {
			t.Errorf("Count(%q) = %d, want 1", s, got)
		}
	}
}

func TestCount_SumsSingleTokenChunks(t *testing.T) {
	tk := mustLoad(t)
	// For a string whose every pre-token chunk is itself a vocab entry,
	// Count must equal the chunk count — the merge neither drops a chunk
	// nor splits a whole-vocab chunk.
	s := "hello world"
	chunks := tk.preTokenize(s)
	for _, c := range chunks {
		if _, ok := tk.ranks[c]; !ok {
			t.Skipf("chunk %q is not a single vocab entry — premise n/a", c)
		}
	}
	if got := tk.Count(s); got != len(chunks) {
		t.Errorf("Count(%q) = %d, want %d (each of %d chunks is one vocab token)",
			s, got, len(chunks), len(chunks))
	}
}

func TestCount_Deterministic(t *testing.T) {
	tk := mustLoad(t)
	s := "The quick brown fox jumps over the lazy dog. 12345! \t\n  done."
	if a, b := tk.Count(s), tk.Count(s); a != b {
		t.Errorf("Count not deterministic: %d vs %d", a, b)
	}
}

func TestCount_MergeNeverIncreasesCount(t *testing.T) {
	tk := mustLoad(t)
	// BPE merges only reduce piece count, so Count(a+b) <= Count(a)+Count(b).
	a, b := "func calculateTotal(", "items []Order) float64 {"
	ca, cb, cab := tk.Count(a), tk.Count(b), tk.Count(a+b)
	if cab > ca+cb {
		t.Errorf("Count(a+b)=%d > Count(a)+Count(b)=%d — a merge increased the count", cab, ca+cb)
	}
	if ca <= 0 {
		t.Error("Count of a non-empty string must be positive")
	}
}

func TestCount_LongChunkBounded(t *testing.T) {
	tk := mustLoad(t)
	// A pathologically long pre-token chunk (a 50k-char separator line) must
	// still return a finite, positive count quickly — the windowing guard.
	s := strings.Repeat("=", 50000)
	if got := tk.Count(s); got <= 0 {
		t.Errorf("Count of a 50k separator = %d, want > 0", got)
	}
}
