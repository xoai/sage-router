package compress

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// M4 T3 — the filter catalog loads + validates fail-closed (AC2), and each
// filter transforms without corrupting structured output (AC3).

func TestLoadCatalog(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog (the embedded filters.json must be valid): %v", err)
	}
	if c == nil || len(c.filters) == 0 {
		t.Fatal("LoadCatalog returned an empty catalog")
	}
}

func TestParseCatalog_Malformed(t *testing.T) {
	cases := []struct{ name, data string }{
		{"bad json", `{not json`},
		{"empty array", `[]`},
		{"missing name", `[{"kind":"ansi-strip","pattern":"x"}]`},
		{"unknown kind", `[{"name":"x","kind":"bogus"}]`},
		{"bad regex", `[{"name":"x","kind":"ansi-strip","pattern":"["}]`},
		{"ansi-strip no pattern", `[{"name":"x","kind":"ansi-strip"}]`},
		{"collapse bad min_run", `[{"name":"x","kind":"collapse-repeats","min_run":1}]`},
		{"truncate bad max_bytes", `[{"name":"x","kind":"truncate-safe","max_bytes":0}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCatalog([]byte(tc.data)); err == nil {
				t.Errorf("parseCatalog(%s): expected a typed error, got nil", tc.name)
			}
		})
	}
}

func mustFilter(t *testing.T, kind string) filter {
	t.Helper()
	c, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	for _, f := range c.filters {
		if f.spec.Kind == kind {
			return f
		}
	}
	t.Fatalf("no filter of kind %q in the embedded catalog", kind)
	return filter{}
}

func TestFilter_ANSIStrip(t *testing.T) {
	f := mustFilter(t, "ansi-strip")
	in := "\x1b[31mERROR\x1b[0m: build failed"
	if got, want := f.apply(in), "ERROR: build failed"; got != want {
		t.Errorf("ansi-strip: got %q, want %q", got, want)
	}
}

func TestFilter_CollapseRepeats(t *testing.T) {
	f := mustFilter(t, "collapse-repeats")
	in := "start\nsame\nsame\nsame\nsame\nend"
	got := f.apply(in)
	if strings.Count(got, "same") != 1 {
		t.Errorf("collapse-repeats: expected one surviving 'same' line, got %q", got)
	}
	if !strings.Contains(got, "start") || !strings.Contains(got, "end") {
		t.Errorf("collapse-repeats: dropped a surrounding non-repeated line: %q", got)
	}
	// A run below min_run is left intact.
	in2 := "a\nb\nb\nc"
	if got2 := f.apply(in2); got2 != in2 {
		t.Errorf("collapse-repeats: collapsed a run shorter than min_run: %q", got2)
	}
}

func TestFilter_TruncateSafe(t *testing.T) {
	f := mustFilter(t, "truncate-safe")
	in := strings.Repeat("a line of build output\n", 5000)
	got := f.apply(in)
	if len(got) >= len(in) {
		t.Errorf("truncate-safe: did not shrink (%d -> %d)", len(in), len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("truncate-safe: produced invalid UTF-8")
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("truncate-safe: missing truncation marker: ...%q", got[max0(len(got)-30):])
	}
	// Content under the cap is untouched.
	small := "short output\n"
	if got := f.apply(small); got != small {
		t.Errorf("truncate-safe: altered under-cap content: %q", got)
	}
}

func TestFilter_TruncateSafe_NoMidRune(t *testing.T) {
	f := mustFilter(t, "truncate-safe")
	// Multibyte runes, no newline in the budget — the cut must back off to a
	// rune boundary, never emit invalid UTF-8.
	in := strings.Repeat("héllo wörld ", 100000) // é, ö are 2 bytes each
	if got := f.apply(in); !utf8.ValidString(got) {
		t.Error("truncate-safe: split a UTF-8 rune — output is not valid UTF-8")
	}
}

func TestFilter_NoiseShortCircuit(t *testing.T) {
	f := mustFilter(t, "noise-shortcircuit")
	in := "compiling project\n[====>     ] 45%\nlinking done"
	got := f.apply(in)
	if strings.Contains(got, "45%") {
		t.Errorf("noise-shortcircuit: a progress-bar line survived: %q", got)
	}
	if !strings.Contains(got, "compiling") || !strings.Contains(got, "linking done") {
		t.Errorf("noise-shortcircuit: removed real content: %q", got)
	}
}

// AC3 — a normal (multi-line, pretty-printed) JSON tool-result triggers no
// filter and survives byte-identical and parseable.
func TestCatalog_JSONUntouched(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	jsonResult := `{
  "status": "ok",
  "items": [
    {"id": 1, "name": "alpha"},
    {"id": 2, "name": "beta"}
  ],
  "count": 2
}`
	got := c.Apply(jsonResult)
	if got != jsonResult {
		t.Errorf("catalog corrupted a normal JSON tool-result:\n in:  %q\n out: %q", jsonResult, got)
	}
	if !json.Valid([]byte(got)) {
		t.Error("catalog output is not valid JSON")
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
