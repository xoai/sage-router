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
