// Package compress is the M4 tool-output compression subsystem (cycle
// 20260522-m4-compression, ADR decision-compression-subsystem.md). It shrinks
// low-signal tool-result content (test output, build logs, file dumps) on
// compression-enabled API keys, before the request is translated upstream.
//
// It is opt-in, tool-result-only, deterministic, and fail-closed: a
// tokenizer or catalog load failure disables compression rather than break
// the server.
package compress

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
)

//go:embed filters.json
var filtersJSON []byte

// Filter transform kinds. parseCatalog validates every entry's kind against
// this closed set — an unknown kind fails the load.
const (
	kindANSIStrip         = "ansi-strip"
	kindCollapseRepeats   = "collapse-repeats"
	kindNoiseShortCircuit = "noise-shortcircuit"
	kindTruncateSafe      = "truncate-safe"
)

// filterSpec is one raw catalog entry as stored in filters.json.
type filterSpec struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Pattern     string `json:"pattern,omitempty"`
	Replacement string `json:"replacement,omitempty"`
	MinRun      int    `json:"min_run,omitempty"`
	MaxBytes    int    `json:"max_bytes,omitempty"`
}

// filter is a validated, compiled catalog entry.
type filter struct {
	spec filterSpec
	re   *regexp.Regexp // compiled Pattern; nil for kinds that use no regex
}

// Catalog is the ordered set of tool-output filters.
type Catalog struct {
	filters []filter
}

// LoadCatalog parses and validates the embedded filter catalog. It is
// fail-closed: an unknown kind, a bad regex, a missing required field, or an
// empty catalog is a typed error — the caller disables compression rather
// than run a broken catalog.
func LoadCatalog() (*Catalog, error) {
	return parseCatalog(filtersJSON)
}

func parseCatalog(data []byte) (*Catalog, error) {
	var specs []filterSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, fmt.Errorf("compress: filter catalog: %w", err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("compress: filter catalog is empty")
	}
	c := &Catalog{}
	for i, s := range specs {
		if s.Name == "" {
			return nil, fmt.Errorf("compress: filter[%d]: missing name", i)
		}
		f := filter{spec: s}
		switch s.Kind {
		case kindANSIStrip, kindNoiseShortCircuit:
			if s.Pattern == "" {
				return nil, fmt.Errorf("compress: filter %q (%s): missing pattern", s.Name, s.Kind)
			}
			re, err := regexp.Compile(s.Pattern)
			if err != nil {
				return nil, fmt.Errorf("compress: filter %q: bad pattern: %w", s.Name, err)
			}
			f.re = re
		case kindCollapseRepeats:
			if s.MinRun < 2 {
				return nil, fmt.Errorf("compress: filter %q: min_run must be >= 2, got %d", s.Name, s.MinRun)
			}
		case kindTruncateSafe:
			if s.MaxBytes < 1 {
				return nil, fmt.Errorf("compress: filter %q: max_bytes must be >= 1, got %d", s.Name, s.MaxBytes)
			}
		default:
			return nil, fmt.Errorf("compress: filter %q: unknown kind %q", s.Name, s.Kind)
		}
		c.filters = append(c.filters, f)
	}
	return c, nil
}

// Apply runs every filter, in catalog order, over text. A nil Catalog
// returns text unchanged (fail-closed).
func (c *Catalog) Apply(text string) string {
	if c == nil {
		return text
	}
	for _, f := range c.filters {
		text = f.apply(text)
	}
	return text
}
