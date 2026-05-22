package tokenizer

import (
	"crypto/sha256"
	"encoding/hex"
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
