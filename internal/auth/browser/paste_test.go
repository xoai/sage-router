package browser

import (
	"bytes"
	"strings"
	"testing"
)

func TestParsePastedRedirect_FullLocalhostURL(t *testing.T) {
	in := "http://localhost:1455/auth/callback?code=abc123&state=deadbeef"
	code, state, err := ParsePastedRedirect(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != "abc123" {
		t.Errorf("code = %q, want abc123", code)
	}
	if state != "deadbeef" {
		t.Errorf("state = %q, want deadbeef", state)
	}
}

func TestParsePastedRedirect_AnthropicCallback(t *testing.T) {
	in := "http://localhost:53692/callback?state=stt&code=cde"
	code, state, err := ParsePastedRedirect(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != "cde" || state != "stt" {
		t.Errorf("got code=%q state=%q", code, state)
	}
}

func TestParsePastedRedirect_BareQueryFragment(t *testing.T) {
	in := "code=xyz789&state=ffeeddcc"
	code, state, err := ParsePastedRedirect(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != "xyz789" {
		t.Errorf("code = %q, want xyz789", code)
	}
	if state != "ffeeddcc" {
		t.Errorf("state = %q, want ffeeddcc", state)
	}
}

func TestParsePastedRedirect_WhitespaceTolerant(t *testing.T) {
	in := "   http://localhost:1455/auth/callback?code=abc&state=def   \n"
	code, state, err := ParsePastedRedirect(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != "abc" || state != "def" {
		t.Errorf("got code=%q state=%q", code, state)
	}
}

func TestParsePastedRedirect_MissingCode(t *testing.T) {
	if _, _, err := ParsePastedRedirect("http://localhost:1455/cb?state=x"); err == nil {
		t.Error("expected error when code is missing")
	}
}

func TestParsePastedRedirect_MissingState(t *testing.T) {
	if _, _, err := ParsePastedRedirect("http://localhost:1455/cb?code=x"); err == nil {
		t.Error("expected error when state is missing")
	}
}

func TestParsePastedRedirect_GarbageInput(t *testing.T) {
	for _, in := range []string{
		"",
		"not even a url or query",
		"foo bar baz",
	} {
		if _, _, err := ParsePastedRedirect(in); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestPromptPasteRedirect_HappyPathFirstAttempt(t *testing.T) {
	stdin := strings.NewReader("http://localhost:1455/auth/callback?code=abc&state=def\n")
	var stdout bytes.Buffer
	code, state, err := PromptPasteRedirect(stdin, &stdout, "https://auth.example.com/...")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != "abc" || state != "def" {
		t.Errorf("got code=%q state=%q", code, state)
	}
	if !strings.Contains(stdout.String(), "https://auth.example.com/...") {
		t.Errorf("prompt should include the authorize URL; got %q", stdout.String())
	}
}

func TestPromptPasteRedirect_RetriesUpToThreeTimes(t *testing.T) {
	// Two bad attempts followed by a good one — should succeed on the third.
	stdin := strings.NewReader("garbage\nstill bad\nhttp://localhost:1455/cb?code=ok&state=stt\n")
	var stdout bytes.Buffer
	code, state, err := PromptPasteRedirect(stdin, &stdout, "https://auth.example.com/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != "ok" || state != "stt" {
		t.Errorf("got code=%q state=%q", code, state)
	}
	// Output should reflect the retries.
	if !strings.Contains(stdout.String(), "could not parse") {
		t.Errorf("expected user-facing retry hint; got %q", stdout.String())
	}
}

func TestPromptPasteRedirect_GivesUpAfterThreeFailures(t *testing.T) {
	stdin := strings.NewReader("bad\nbad\nbad\n")
	var stdout bytes.Buffer
	_, _, err := PromptPasteRedirect(stdin, &stdout, "https://auth.example.com/")
	if err == nil {
		t.Fatal("expected error after 3 bad attempts")
	}
}

func TestPromptPasteRedirect_EOFWithoutInputErrors(t *testing.T) {
	stdin := strings.NewReader("")
	var stdout bytes.Buffer
	_, _, err := PromptPasteRedirect(stdin, &stdout, "https://auth.example.com/")
	if err == nil {
		t.Fatal("expected error on EOF")
	}
}
