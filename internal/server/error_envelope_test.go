package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Carryover #32a — codify the writeError envelope shape contract.
//
// Frontend api/client.js parses error responses as `.error.message`
// (post-#31). Any future refactor of writeError that changes the
// envelope shape would silently break the frontend. This Go-side
// contract test pins what writeError emits today so a regression
// fails server-side before it reaches the browser.
//
// JS-side mirror test is deferred to item #30 (Vitest infrastructure
// not yet stood up); the Go-side contract is the load-bearing half
// since the frontend cannot test what the server doesn't emit.

// TestWriteError_EnvelopeShape calls writeError directly and asserts
// the JSON body parses to the expected envelope shape:
//
//	{"error": {"message": <string>, "type": <string>}}
//
// Frontend's request() helper at web/dashboard/src/api/client.js
// destructures `.error.message`. The `type` field is informational
// but kept stable so future fields can be added under `.error.*`
// without shape-breaking changes.
func TestWriteError_EnvelopeShape(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
	}{
		{"client error", http.StatusBadRequest, "missing required field"},
		{"unauthorized", http.StatusUnauthorized, "auth required"},
		{"not found", http.StatusNotFound, "no such resource"},
		{"server error", http.StatusInternalServerError, "internal failure"},
		{"message with quotes", http.StatusBadRequest, `field "x" rejected`},
		{"empty message", http.StatusBadRequest, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeError(rec, c.status, c.message)

			if rec.Code != c.status {
				t.Errorf("status = %d, want %d", rec.Code, c.status)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}

			// Decode into a generic shape — explicitly NOT a typed struct,
			// so a regression that adds an extra wrapper layer (e.g.
			// {"data": {"error": ...}}) is detected.
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v; body: %s", err, rec.Body.String())
			}

			errField, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("body.error is not an object; body: %s", rec.Body.String())
			}
			gotMessage, ok := errField["message"].(string)
			if !ok {
				t.Fatalf("body.error.message missing or not a string; body: %s", rec.Body.String())
			}
			if gotMessage != c.message {
				t.Errorf("body.error.message = %q, want %q", gotMessage, c.message)
			}
			if _, ok := errField["type"].(string); !ok {
				t.Errorf("body.error.type missing or not a string; body: %s", rec.Body.String())
			}

			// Pin the negative shape: no top-level `message`, no top-
			// level `errors` (plural), no `data` wrapper. The frontend's
			// destructuring at api/client.js depends on `error` being the
			// single top-level key.
			for _, forbidden := range []string{"message", "errors", "data"} {
				if _, present := body[forbidden]; present {
					t.Errorf("body must not have top-level %q; the envelope is {error: {...}} only", forbidden)
				}
			}
		})
	}
}
