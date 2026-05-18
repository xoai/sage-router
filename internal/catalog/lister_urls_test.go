package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sage-router/internal/config"
)

// URL-composition regression — initiative 20260514-discovery-url-doubling.
//
// Every lister appends a resource path to creds.BaseURL. In production,
// BaseURL comes from config.KnownProviders[*].BaseURL and already
// includes the API version segment (/v1, /api/v1, /v1beta, /api).
// Pre-fix, the listers appended a SECOND version segment, producing
// non-existent paths like /v1/v1/models — every discovery call 404'd
// in production for ~3 months while passing tests that used bare
// httptest.NewServer URLs (no path prefix).
//
// This file is the centralized regression guard: for each (provider,
// lister) pair, set up a test server, build a BaseURL that mimics
// production shape (test-server-base + version-segment-from-config),
// invoke the lister, assert the constructed path matches the canonical
// upstream form EXACTLY (no doubled segment, no missing segment).

// TestListerURLs_NoDoubledVersionSegment is the table-driven regression
// guard for the URL-doubling bug. Each row injects a production-shape
// BaseURL by concatenating the test-server base with the version path
// taken from config.KnownProviders — this is the configuration the
// production code uses, and the one that exposes the doubling bug.
func TestListerURLs_NoDoubledVersionSegment(t *testing.T) {
	cases := []struct {
		provider   string
		lister     ListerFunc
		baseSuffix string // version segment from config.KnownProviders (e.g., "/v1")
		wantPath   string // canonical upstream resource path
		body       string // minimal valid response so the lister doesn't return an error
	}{
		{
			provider:   "openai",
			lister:     listOpenAIModels,
			baseSuffix: "/v1",
			wantPath:   "/v1/models",
			body:       `{"object":"list","data":[]}`,
		},
		{
			provider:   "anthropic",
			lister:     listAnthropicModels,
			baseSuffix: "/v1",
			wantPath:   "/v1/models",
			body:       `{"data":[],"has_more":false}`,
		},
		{
			provider:   "openrouter",
			lister:     listOpenRouterModels,
			baseSuffix: "/api/v1",
			wantPath:   "/api/v1/models",
			body:       `{"data":[]}`,
		},
		{
			provider:   "gemini",
			lister:     listGeminiModels,
			baseSuffix: "/v1beta",
			wantPath:   "/v1beta/models",
			body:       `{"models":[]}`,
		},
		{
			provider:   "ollama",
			lister:     listOllamaModels,
			baseSuffix: "/api",
			wantPath:   "/api/tags",
			body:       `{"models":[]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			// Sanity check the test setup against config.KnownProviders:
			// the baseSuffix MUST be the trailing path of the production
			// BaseURL. If the production config changes, this test must
			// be updated in lockstep.
			prodBase := config.KnownProviders[tc.provider].BaseURL
			if !strings.HasSuffix(prodBase, tc.baseSuffix) {
				t.Fatalf("test setup invalid: config.KnownProviders[%q].BaseURL = %q does not end with %q",
					tc.provider, prodBase, tc.baseSuffix)
			}

			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			// Production-shape BaseURL: test-server base + version segment.
			// E.g., for openai: "http://127.0.0.1:38291" + "/v1".
			productionShapeBaseURL := srv.URL + tc.baseSuffix

			_, err := tc.lister(context.Background(), ListerCredentials{
				BaseURL: productionShapeBaseURL,
				APIKey:  "test-key-for-url-shape",
			})
			if err != nil {
				t.Fatalf("lister returned error: %v", err)
			}

			if gotPath != tc.wantPath {
				t.Errorf("constructed path = %q, want %q (URL-doubling regression — see plan 20260514-discovery-url-doubling)",
					gotPath, tc.wantPath)
			}
		})
	}
}
