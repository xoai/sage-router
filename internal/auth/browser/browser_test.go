package browser

import (
	"errors"
	"testing"
)

func TestCommandForGOOS_Platforms(t *testing.T) {
	tests := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{"linux", "xdg-open", []string{"https://example.com"}},
		{"darwin", "open", []string{"https://example.com"}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", "https://example.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			name, args, err := commandForGOOS(tc.goos, "https://example.com")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args len = %d, want %d", len(args), len(tc.wantArgs))
			}
			for i, a := range args {
				if a != tc.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, a, tc.wantArgs[i])
				}
			}
		})
	}
}

func TestCommandForGOOS_UnsupportedReturnsError(t *testing.T) {
	if _, _, err := commandForGOOS("plan9", "https://example.com"); err == nil {
		t.Error("plan9 should be rejected")
	}
}

func TestOpen_HappyPath_UsesInjectedStarter(t *testing.T) {
	// Swap the package-level starter with a recording fake so we can
	// verify Open didn't actually fork a process during the test.
	var gotName string
	var gotArgs []string
	orig := startBrowser
	startBrowser = func(name string, args []string) error {
		gotName = name
		gotArgs = args
		return nil
	}
	defer func() { startBrowser = orig }()

	if err := Open("https://auth.example.com/oauth/authorize?x=1"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if gotName == "" {
		t.Error("starter was not invoked")
	}
	// Last arg is always the URL on every platform.
	if gotArgs[len(gotArgs)-1] != "https://auth.example.com/oauth/authorize?x=1" {
		t.Errorf("last arg = %q, want the URL", gotArgs[len(gotArgs)-1])
	}
}

func TestOpen_StartFailurePropagatesWrappedError(t *testing.T) {
	orig := startBrowser
	startBrowser = func(string, []string) error {
		return errors.New("exec failed")
	}
	defer func() { startBrowser = orig }()

	err := Open("https://example.com")
	if err == nil {
		t.Fatal("expected error when starter fails")
	}
	if !errorContains(err, "browser open") || !errorContains(err, "exec failed") {
		t.Errorf("error should wrap both context and underlying cause; got %q", err.Error())
	}
}

func errorContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for i := 0; i+len(substr) <= len(msg); i++ {
		if msg[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
