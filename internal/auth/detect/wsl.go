package detect

// wslDistros lists the WSL distribution names probed when sage-router
// runs on Windows but the user installed an upstream CLI (Claude Code,
// Codex CLI) inside WSL. Walking these paths from the Windows side
// lets the dashboard auto-detect credentials regardless of which
// platform the CLI was installed on.
//
// The list is shared between detect/claude.go and detect/codex.go via
// `claudeCredPaths()` and `codexCredPaths()`. It is mutable so tests
// can disable WSL probing entirely via `DisableWSLForTesting`. Without
// that override, tests on a Windows-with-WSL developer machine that
// has the corresponding CLI installed in WSL would read the REAL
// credentials regardless of HOME/USERPROFILE stubs, because the WSL
// paths are constructed from hardcoded distro names rather than env
// vars.
//
// Production code never mutates this; only tests do.
var wslDistros = []string{"Ubuntu", "Ubuntu-22.04", "Ubuntu-24.04", "Debian", "kali-linux"}

// DisableWSLForTesting clears the WSL distribution probe list and
// returns a restore function. Tests in this and other packages should
// call this when stubbing the filesystem for DetectClaude/DetectCodex
// — otherwise Windows-runtime tests on machines with WSL CLIs leak
// real credentials into the test.
//
// Usage:
//
//	restore := detect.DisableWSLForTesting()
//	defer restore()
//
// Or with testing.TB.Cleanup:
//
//	t.Cleanup(detect.DisableWSLForTesting())
//
// Production code MUST NOT call this — it would silently disable the
// WSL-detection feature that some Windows users rely on.
func DisableWSLForTesting() func() {
	saved := wslDistros
	wslDistros = nil
	return func() { wslDistros = saved }
}
