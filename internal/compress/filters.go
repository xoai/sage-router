package compress

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// apply runs one filter's transform over text. The kind is already validated
// by parseCatalog, so the default arm is unreachable.
func (f filter) apply(text string) string {
	switch f.spec.Kind {
	case kindANSIStrip:
		return f.re.ReplaceAllString(text, "")
	case kindNoiseShortCircuit:
		return f.re.ReplaceAllString(text, f.spec.Replacement)
	case kindCollapseRepeats:
		return collapseRepeats(text, f.spec.MinRun)
	case kindTruncateSafe:
		return truncateSafe(text, f.spec.MaxBytes)
	default:
		return text
	}
}

// collapseRepeats replaces a run of minRun-or-more identical adjacent lines
// with one copy of the line plus a count marker. Shorter runs are untouched.
func collapseRepeats(text string, minRun int) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		run := j - i
		out = append(out, lines[i])
		if run >= minRun {
			out = append(out, fmt.Sprintf("... [x%d identical lines]", run))
		} else {
			for k := i + 1; k < j; k++ {
				out = append(out, lines[k])
			}
		}
		i = j
	}
	return strings.Join(out, "\n")
}

// truncateSafe truncates text to at most maxBytes, cutting at the last line
// boundary within the budget — and if there is none, backing off to a valid
// UTF-8 rune boundary. It never emits invalid UTF-8 and never cuts a line
// (or a JSON token) mid-way. Content already within the budget is returned
// unchanged.
func truncateSafe(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := text[:maxBytes]
	if nl := strings.LastIndexByte(cut, '\n'); nl >= 0 {
		// A line boundary is inherently a valid UTF-8 boundary ('\n' is a
		// single byte that cannot be part of a multibyte rune).
		cut = cut[:nl]
	} else {
		// No line boundary in the budget — back off to a rune boundary.
		// Canonical request content is JSON-decoded and therefore valid
		// UTF-8, so the only mid-rune split is the cut itself; at most
		// utf8.UTFMax-1 backoff bytes are ever needed.
		for n := 0; n < utf8.UTFMax && len(cut) > 0 && !utf8.ValidString(cut); n++ {
			cut = cut[:len(cut)-1]
		}
	}
	return cut + "\n... [truncated]"
}
