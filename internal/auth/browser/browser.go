package browser

import (
	"fmt"
	"os/exec"
	"runtime"
)

// startBrowser is the package-level hook for cmd.Start so tests can swap
// in a fake without an actual subprocess. Production code uses the
// default which calls exec.Command(...).Start().
var startBrowser = defaultStartBrowser

// commandForGOOS returns the (name, args) tuple for the platform's
// browser-open command. Exported (lowercase) for tests in the same
// package so they can assert the platform-specific argv shape.
func commandForGOOS(goos, targetURL string) (string, []string, error) {
	switch goos {
	case "linux":
		return "xdg-open", []string{targetURL}, nil
	case "darwin":
		return "open", []string{targetURL}, nil
	case "windows":
		// rundll32 is the most portable way to open the default browser
		// on Windows without needing PowerShell or cmd quoting gymnastics.
		return "rundll32", []string{"url.dll,FileProtocolHandler", targetURL}, nil
	default:
		return "", nil, fmt.Errorf("unsupported GOOS %q", goos)
	}
}

func defaultStartBrowser(name string, args []string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Intentionally do NOT cmd.Wait() — the browser launches in the
	// background and the OAuth flow proceeds independently. cmd.Process
	// will be reaped by the OS once it exits.
	return nil
}

// Open launches the user's default browser to the given URL using the
// platform-appropriate command. Returns an error when the command can't
// be located or fails to start; callers should fall through to
// PromptPasteRedirect in that case.
func Open(targetURL string) error {
	name, args, err := commandForGOOS(runtime.GOOS, targetURL)
	if err != nil {
		return fmt.Errorf("browser open: %w", err)
	}
	if err := startBrowser(name, args); err != nil {
		return fmt.Errorf("browser open: %w", err)
	}
	return nil
}
