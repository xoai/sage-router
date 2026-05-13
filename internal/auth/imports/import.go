// Package imports parses credential files written by upstream LLM CLI tools
// (Codex, Claude Code, GitHub Copilot, Gemini CLI) into auth.Credential
// values that sage-router can use as subscription connections.
//
// Each parser is independent and can be invoked directly; the [ImportFromCLI]
// dispatcher selects by canonical provider ID. All parsers normalize epoch
// formats (some providers use ms-since-epoch, some use seconds) and validate
// that an access_token (or refresh_token, for Copilot's 2-step flow) is
// present.
package imports

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// ErrFileNotFound is returned when the provider's CLI credential file
// doesn't exist on disk. Callers (CLI / dashboard route) translate this
// to a 404 with a helpful hint about running the provider's CLI first.
var ErrFileNotFound = errors.New("imports: credential file not found")

// ErrEmptyToken signals that the file existed and parsed but contained no
// usable token material. Distinct from a parse error so callers can
// surface a more specific message ("file looks empty — re-run the CLI?").
var ErrEmptyToken = errors.New("imports: file contains no access or refresh token")

// Parser is the per-provider parse function.
type Parser func(path string) (*auth.Credential, error)

// parsers is the registry. Keyed by canonical provider ID.
var parsers = map[string]Parser{
	"openai":         parseCodexFile,
	"anthropic":      parseClaudeFile,
	"github-copilot": parseCopilotFile,
	"gemini":         parseGeminiFile,
}

// ImportFromCLI reads the provider's credential file from disk (path
// resolved via the registry, with $ENV override support) and returns a
// fresh auth.Credential. The returned Credential's Provider is set to the
// canonical ID; ConnectionID is left empty for the caller to assign.
//
// Pass an explicit overridePath (non-empty) to bypass the registry's
// default location and env-var lookup — useful for tests and for the
// CLI's --path flag.
func ImportFromCLI(provider, overridePath string) (*auth.Credential, error) {
	cfg, ok := providers.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("imports: unknown provider %q", provider)
	}
	parser, ok := parsers[provider]
	if !ok {
		return nil, fmt.Errorf("imports: no parser registered for %q", provider)
	}

	path := overridePath
	if path == "" {
		path = resolvePath(cfg)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s", ErrFileNotFound, path)
	}

	cred, err := parser(path)
	if err != nil {
		return nil, err
	}
	cred.Provider = provider
	return cred, nil
}

// MacOSClaudeKeychainHint reports whether the absence of the Claude file
// on macOS likely means the credentials live in Keychain instead. AC18
// branch condition: file missing + GOOS=darwin. Callers compose this with
// the actual file existence check.
func MacOSClaudeKeychainHint(err error) bool {
	return errors.Is(err, ErrFileNotFound) && runtime.GOOS == "darwin"
}

// KeychainHintMessage is the user-facing copy shown when the macOS
// Keychain hint fires. Stored as a constant so the CLI and dashboard
// surface identical wording.
const KeychainHintMessage = "Claude Code on macOS stores credentials in the system Keychain " +
	"rather than a file. Set CLAUDE_CODE_OAUTH_TOKEN as an environment " +
	"variable as a workaround, or use `auth login --provider anthropic` " +
	"to perform the OAuth flow directly."

// resolvePath expands ~ and applies the provider's env-var override (e.g.,
// $CODEX_HOME, $COPILOT_HOME). The env var, if set, replaces the directory
// portion of ImportPath — e.g., $CODEX_HOME=/tmp/codex points
// ~/.codex/auth.json at /tmp/codex/auth.json.
func resolvePath(cfg providers.ProviderConfig) string {
	path := cfg.ImportPath
	if cfg.ImportPathEnvVar != "" {
		if v := os.Getenv(cfg.ImportPathEnvVar); v != "" {
			// Replace the leading "~/.<dirname>" with the env-var value.
			// For "~/.codex/auth.json" + CODEX_HOME=/tmp/codex, result is "/tmp/codex/auth.json".
			path = filepath.Join(v, filepath.Base(cfg.ImportPath))
		}
	}
	return expandTilde(path)
}

func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
