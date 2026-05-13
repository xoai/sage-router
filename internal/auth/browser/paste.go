// Package browser implements the platform-specific browser-open helper used
// to launch the user into a provider's OAuth authorize URL, plus a paste-
// redirect fallback for headless environments (SSH sessions, WSL without
// $BROWSER, CI smoke tests).
package browser

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

const maxPasteAttempts = 3

// ParsePastedRedirect extracts (code, state) from one of:
//   - a full URL (typically the localhost callback that the user's browser
//     was redirected to and could not load):
//       http://localhost:1455/auth/callback?code=X&state=Y
//   - a bare query fragment:
//       code=X&state=Y
//
// Whitespace at the input edges is trimmed. Missing code OR missing state
// is an error.
//
// SECURITY BOUNDARY (auto-review note from spec Task 2.10): this function
// performs NO authenticity validation on `state`. State authenticity is
// enforced downstream by Bridge.Consume (M2.5), which rejects unknown
// states with HTTP 400. ParsePastedRedirect is intentionally permissive
// so the headless paste-redirect flow works across machines (the browser
// may have failed to actually load the localhost URL — we only need the
// query string).
func ParsePastedRedirect(input string) (code, state string, err error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return "", "", errors.New("empty input")
	}

	var rawQuery string
	if strings.Contains(in, "://") {
		// Full URL: parse and extract RawQuery.
		u, perr := url.Parse(in)
		if perr != nil {
			return "", "", fmt.Errorf("parse URL: %w", perr)
		}
		rawQuery = u.RawQuery
	} else if strings.Contains(in, "=") {
		// Bare query fragment.
		rawQuery = in
	} else {
		return "", "", errors.New("input is neither a URL nor a code/state query string")
	}

	values, perr := url.ParseQuery(rawQuery)
	if perr != nil {
		return "", "", fmt.Errorf("parse query: %w", perr)
	}
	code = values.Get("code")
	state = values.Get("state")
	if code == "" {
		return "", "", errors.New("missing 'code' parameter")
	}
	if state == "" {
		return "", "", errors.New("missing 'state' parameter")
	}
	return code, state, nil
}

// PromptPasteRedirect drives an interactive paste-redirect on stdout/stdin.
// It prints the authorize URL and prompts the user to paste the redirect
// URL their browser was directed to. Up to 3 attempts; returns the
// (code, state) pair from the first valid input.
//
// Cross-machine support (AC49): the redirect URL the user pastes will
// point at localhost:{port}, which is unreachable from a different host.
// That's expected — the user copies the URL from their browser's address
// bar (where the browser failed to load the localhost callback) and pastes
// it here. We only need the query string.
func PromptPasteRedirect(stdin io.Reader, stdout io.Writer, authorizeURL string) (code, state string, err error) {
	fmt.Fprintf(stdout, "Open this URL in any browser, complete the authorization,\n")
	fmt.Fprintf(stdout, "then paste the FULL redirect URL (or just the code+state query):\n\n")
	fmt.Fprintf(stdout, "  %s\n\n", authorizeURL)
	fmt.Fprintf(stdout, "Note: the browser will likely fail to load the redirect URL\n")
	fmt.Fprintf(stdout, "(it points at localhost). Copy the URL from the address bar\n")
	fmt.Fprintf(stdout, "and paste it back here — that's expected.\n\n")

	scanner := bufio.NewScanner(stdin)
	// OAuth callback URLs can be long with JWTs etc.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for attempt := 1; attempt <= maxPasteAttempts; attempt++ {
		fmt.Fprintf(stdout, "Paste redirect URL (attempt %d/%d): ", attempt, maxPasteAttempts)
		if !scanner.Scan() {
			// EOF before any input — bail out cleanly.
			if serr := scanner.Err(); serr != nil {
				return "", "", fmt.Errorf("read input: %w", serr)
			}
			return "", "", errors.New("no input received")
		}
		c, s, parseErr := ParsePastedRedirect(scanner.Text())
		if parseErr == nil {
			return c, s, nil
		}
		fmt.Fprintf(stdout, "  could not parse: %v\n", parseErr)
	}
	return "", "", fmt.Errorf("paste-redirect: %d attempts failed", maxPasteAttempts)
}
