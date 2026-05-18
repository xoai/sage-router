package executor

import (
	"context"
	"errors"
	"testing"

	"sage-router/pkg/canonical"
)

// fullVariant implements all four optional interfaces. Used to verify
// that RetryExecutor's wrap preserves each interface's behavior.
type fullVariant struct{}

func (fullVariant) Provider() string { return "full" }
func (fullVariant) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	return &Result{StatusCode: 200}, nil
}
func (fullVariant) Format() canonical.Format          { return canonical.FormatResponses }
func (fullVariant) NeedsOAuthIdentity() bool          { return true }
func (fullVariant) ParseAuthError(s int, b []byte) error {
	if s == 401 {
		return errors.New("tier-error from full")
	}
	return nil
}
func (fullVariant) PreflightCredentials(c *Credentials) error {
	if c == nil || c.AccessToken == "" {
		return errors.New("missing access token")
	}
	return nil
}

// bareVariant implements ONLY the required Executor interface. Used to
// verify that the optional-interface helpers return safe defaults when
// the underlying executor doesn't implement them.
type bareVariant struct{}

func (bareVariant) Provider() string { return "bare" }
func (bareVariant) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	return &Result{StatusCode: 200}, nil
}

// TestRetryExecutor_PreservesVariantInterfaces — wrapping a variant that
// implements all four optional interfaces with NewRetryExecutor MUST preserve
// every interface. Per memory `fb0b4ef62`: RetryExecutor MUST explicitly
// delegate optional interfaces, since type-assertions see the outer type
// not the wrapped inner.
func TestRetryExecutor_PreservesVariantInterfaces(t *testing.T) {
	var wrapped Executor = NewRetryExecutor(fullVariant{}, DefaultRetryConfig())

	if f, ok := wrapped.(Formatted); !ok {
		t.Errorf("wrapped does not implement Formatted")
	} else if got := f.Format(); got != canonical.FormatResponses {
		t.Errorf("Format() = %q, want %q", got, canonical.FormatResponses)
	}

	if o, ok := wrapped.(OAuthIdentified); !ok {
		t.Errorf("wrapped does not implement OAuthIdentified")
	} else if !o.NeedsOAuthIdentity() {
		t.Errorf("NeedsOAuthIdentity() = false, want true")
	}

	if p, ok := wrapped.(AuthErrorParser); !ok {
		t.Errorf("wrapped does not implement AuthErrorParser")
	} else {
		if err := p.ParseAuthError(401, []byte("body")); err == nil {
			t.Errorf("ParseAuthError(401) returned nil, want error")
		}
		if err := p.ParseAuthError(200, []byte("body")); err != nil {
			t.Errorf("ParseAuthError(200) returned %v, want nil", err)
		}
	}

	if pc, ok := wrapped.(PreflightChecker); !ok {
		t.Errorf("wrapped does not implement PreflightChecker")
	} else {
		if err := pc.PreflightCredentials(&Credentials{AccessToken: "x"}); err != nil {
			t.Errorf("PreflightCredentials(valid) = %v, want nil", err)
		}
		if err := pc.PreflightCredentials(&Credentials{}); err == nil {
			t.Errorf("PreflightCredentials(empty) returned nil, want error")
		}
	}
}

// TestRetryExecutor_PreservesVariantInterfaces_Nil — wrapping a variant that
// implements NO optional interfaces still allows safe assertion + fallback.
// RetryExecutor declares forwarder methods that detect the inner type doesn't
// implement the optional interface and return safe defaults (empty Format,
// false, nil, nil).
//
// Key behavior: the wrapped value DOES type-assert to the optional interfaces
// (because RetryExecutor declares them as methods on its own type) — the
// methods just return safe-default values. This is by design: helpers like
// formatOf() in routes_v1.go can call wrapped.(Formatted).Format() without
// nil-checking and get a sensible default.
func TestRetryExecutor_PreservesVariantInterfaces_Nil(t *testing.T) {
	var wrapped Executor = NewRetryExecutor(bareVariant{}, DefaultRetryConfig())

	if f, ok := wrapped.(Formatted); !ok {
		t.Errorf("wrapped should still satisfy Formatted (returns empty Format as default)")
	} else if got := f.Format(); got != "" {
		t.Errorf("Format() default = %q, want \"\" (empty Format)", got)
	}

	if o, ok := wrapped.(OAuthIdentified); !ok {
		t.Errorf("wrapped should still satisfy OAuthIdentified (returns false)")
	} else if o.NeedsOAuthIdentity() {
		t.Errorf("NeedsOAuthIdentity() default = true, want false")
	}

	if p, ok := wrapped.(AuthErrorParser); !ok {
		t.Errorf("wrapped should still satisfy AuthErrorParser (returns nil)")
	} else if err := p.ParseAuthError(401, []byte("anything")); err != nil {
		t.Errorf("ParseAuthError(401) default = %v, want nil", err)
	}

	if pc, ok := wrapped.(PreflightChecker); !ok {
		t.Errorf("wrapped should still satisfy PreflightChecker (returns nil)")
	} else if err := pc.PreflightCredentials(&Credentials{}); err != nil {
		t.Errorf("PreflightCredentials() default = %v, want nil", err)
	}
}
