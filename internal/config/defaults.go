package config

import (
	"os"
	"path/filepath"
)

const (
	// AppName is the human-readable application name.
	AppName = "Sage Router"

	// DefaultPort is the default HTTP listen port.
	DefaultPort = 20128

	// DefaultHost is the default bind address.
	DefaultHost = "127.0.0.1"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// DefaultDataDir returns the absolute path to the default data directory
// (~/.sage-router). It expands the home directory at runtime.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".sage-router")
	}
	return filepath.Join(home, ".sage-router")
}

// DefaultDBPath returns the absolute path to the default SQLite database
// (~/.sage-router/sage-router.db).
func DefaultDBPath() string {
	return filepath.Join(DefaultDataDir(), "sage-router.db")
}
