package auth

import "testing"

func TestNormalizeAuthType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Canonical inputs pass through unchanged.
		{"canonical apikey", "apikey", "apikey"},
		{"canonical subscription", "subscription", "subscription"},
		{"canonical auto_detect", "auto_detect", "auto_detect"},
		{"canonical none", "none", "none"},

		// Legacy inputs are normalized.
		{"legacy api_key", "api_key", "apikey"},
		{"legacy oauth", "oauth", "subscription"},

		// Case-insensitive.
		{"upper APIKEY", "APIKEY", "apikey"},
		{"upper OAUTH", "OAUTH", "subscription"},
		{"mixed Api_Key", "Api_Key", "apikey"},
		{"mixed Auto_Detect", "Auto_Detect", "auto_detect"},
		{"mixed NoNe", "NoNe", "none"},

		// Whitespace trimmed.
		{"leading space", " apikey", "apikey"},
		{"trailing space", "subscription ", "subscription"},
		{"tab", "\tapi_key\t", "apikey"},
		{"newline", "oauth\n", "subscription"},

		// Empty maps to none.
		{"empty string", "", "none"},

		// Unknown values pass through unchanged so the executor's
		// default branch can reject them loudly.
		{"unknown passes through", "garbage", "garbage"},
		{"unknown preserved verbatim", "weird_value", "weird_value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeAuthType(tt.in)
			if got != tt.want {
				t.Errorf("NormalizeAuthType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsCanonicalAuthType(t *testing.T) {
	canonical := []string{"apikey", "subscription", "auto_detect", "none"}
	for _, v := range canonical {
		if !IsCanonicalAuthType(v) {
			t.Errorf("IsCanonicalAuthType(%q) = false, want true", v)
		}
	}

	noncanonical := []string{"api_key", "oauth", "APIKEY", "garbage", ""}
	for _, v := range noncanonical {
		if IsCanonicalAuthType(v) {
			t.Errorf("IsCanonicalAuthType(%q) = true, want false", v)
		}
	}
}
