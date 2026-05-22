// Package tokenizer is a vendored, pure-Go o200k_base BPE tokenizer.
//
// It is the one calibrated dependency exception the routing-core-hardening
// brief permits: a //go:embed'd vocabulary table, no cgo, no module
// dependency. M4 uses it for token COUNTS only — the compression pre-flight
// gate and the labeled `tokens_before` measurement (cycle
// 20260522-m4-compression, ADR decision-compression-subsystem.md). It is not
// used for headline savings numbers; `o200k_base` is exact for OpenAI-family
// models and approximate (~10-20%) for Claude/Gemini — acceptable for a
// coarse gate (ADR Gap D).
package tokenizer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed o200k_base.tiktoken
var o200kData []byte

// o200kSHA256 is the SHA-256 of the vendored o200k_base.tiktoken vocab file
// (downloaded from the published tiktoken encodings). It is pinned so a
// truncated or replaced download is caught at load time, not as silent
// token-count drift later (M4 T1, plan-review MI3).
const o200kSHA256 = "446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d"

// expectedRankCount is the number of BPE rank entries in o200k_base
// (special tokens are not in the table — content carries none).
const expectedRankCount = 199998

// Tokenizer holds the o200k_base BPE rank table. Construct it via
// LoadTokenizer; the zero value is not usable.
type Tokenizer struct {
	ranks map[string]int // raw token bytes (as a string) → BPE rank
}

// LoadTokenizer parses the embedded o200k_base vocabulary into a Tokenizer.
//
// It is fail-closed: a checksum mismatch or a malformed table is a typed
// error, never a panic. The compression subsystem disables compression on a
// load error rather than crash the server.
func LoadTokenizer() (*Tokenizer, error) {
	sum := sha256.Sum256(o200kData)
	if got := hex.EncodeToString(sum[:]); got != o200kSHA256 {
		return nil, fmt.Errorf("tokenizer: o200k_base checksum mismatch: got %s, want %s", got, o200kSHA256)
	}
	ranks, err := parseRanks(o200kData)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: %w", err)
	}
	return &Tokenizer{ranks: ranks}, nil
}

// parseRanks parses tiktoken's `<base64-token> <rank>` line format into a
// raw-bytes → rank map.
func parseRanks(data []byte) (map[string]int, error) {
	ranks := make(map[string]int, expectedRankCount)
	sc := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		sp := strings.IndexByte(text, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("line %d: missing space separator", line)
		}
		tok, err := base64.StdEncoding.DecodeString(text[:sp])
		if err != nil {
			return nil, fmt.Errorf("line %d: bad base64 token: %w", line, err)
		}
		rank, err := strconv.Atoi(text[sp+1:])
		if err != nil {
			return nil, fmt.Errorf("line %d: bad rank %q: %w", line, text[sp+1:], err)
		}
		ranks[string(tok)] = rank
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	if len(ranks) == 0 {
		return nil, errors.New("empty rank table")
	}
	return ranks, nil
}

// o200kPattern is the o200k_base pre-tokenization pattern, adapted for Go's
// RE2 engine. tiktoken's verbatim pattern has a `\s+(?!\S)` alternative — a
// negative lookahead RE2 does not support — so that trailing-whitespace
// alternative is dropped; a run of trailing whitespace is then matched by
// the plain `\s+` alternative instead. The boundary of at most one
// whitespace chunk shifts; for a coarse token-count gate that is immaterial
// (ADR Gap D — exact tiktoken parity is explicitly not required).
var o200kPattern = regexp.MustCompile(
	`[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?` +
		`|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?` +
		`|\p{N}{1,3}` +
		`| ?[^\s\p{L}\p{N}]+[\r\n/]*` +
		`|\s*[\r\n]` +
		`|\s+`)

// maxRank is the sentinel for "this byte pair is not a known token". Real
// o200k_base ranks are well below 200000.
const maxRank = 1 << 62

// maxMergeLen bounds the O(n²) byte-pair merge. A pre-token chunk longer
// than this (e.g. a 50k-char separator line — rare, but real in tool logs)
// is windowed: each window is merged independently. Resetting merge state at
// a window boundary over-counts by at most ~1 token per window — immaterial
// for the coarse gate, and it makes Count linear in input length. 512 is
// well above the longest real o200k_base token (~128 bytes), and the
// whole-chunk fast path handles real tokens regardless of this bound; the
// limit only caps the merge cost of pathological boundary-free runs.
const maxMergeLen = 512

// Count returns the o200k_base BPE token count of s. A nil Tokenizer counts
// 0 — a caller whose tokenizer failed to load gets a safe zero, not a panic.
func (t *Tokenizer) Count(s string) int {
	if t == nil || s == "" {
		return 0
	}
	total := 0
	for _, chunk := range t.preTokenize(s) {
		total += t.bpeCount([]byte(chunk))
	}
	return total
}

// preTokenize splits text into o200k_base pre-token chunks. BPE merges never
// cross a chunk boundary.
func (t *Tokenizer) preTokenize(s string) []string {
	return o200kPattern.FindAllString(s, -1)
}

// bpeCount returns the number of BPE tokens a single pre-token chunk merges
// into, against the o200k_base rank table.
func (t *Tokenizer) bpeCount(piece []byte) int {
	if len(piece) == 0 {
		return 0
	}
	if _, ok := t.ranks[string(piece)]; ok {
		return 1 // the whole chunk is a single vocabulary token
	}
	if len(piece) == 1 {
		return 1 // every single byte is a token in o200k_base
	}
	if len(piece) > maxMergeLen {
		// Window a pathologically long chunk to bound the merge cost.
		return t.bpeCount(piece[:maxMergeLen]) + t.bpeCount(piece[maxMergeLen:])
	}

	// parts holds the current segmentation as boundary offsets: the bytes
	// between parts[i] and parts[i+1] are one segment. Start one byte per
	// segment; repeatedly merge the adjacent pair whose concatenation has
	// the lowest BPE rank, until no adjacent pair is a known token.
	parts := make([]int, len(piece)+1)
	for i := range parts {
		parts[i] = i
	}
	pairRank := func(i int) int {
		if i+2 >= len(parts) {
			return maxRank
		}
		if r, ok := t.ranks[string(piece[parts[i]:parts[i+2]])]; ok {
			return r
		}
		return maxRank
	}
	for len(parts) > 2 {
		minRank, minI := maxRank, -1
		for i := 0; i+2 < len(parts); i++ {
			if r := pairRank(i); r < minRank {
				minRank, minI = r, i
			}
		}
		if minI < 0 {
			break // no adjacent pair is a known token — done merging
		}
		parts = append(parts[:minI+1], parts[minI+2:]...)
	}
	return len(parts) - 1
}
