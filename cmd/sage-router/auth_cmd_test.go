package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sage-router/internal/auth"
	"sage-router/internal/store"
)

// newTestEnv returns a cliEnv pointing at an isolated temp dir so
// resolveDataDir doesn't read the user's real ~/.sage-router.
func newTestEnv(t *testing.T) (cliEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	tmp := t.TempDir()
	return cliEnv{
		stdin:       strings.NewReader(""),
		stdout:      stdout,
		stderr:      stderr,
		dataDir:     tmp,
		getEnv:      func(string) string { return "" },
		openBrowser: func(string) error { return nil },
	}, stdout, stderr
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ── resolveMasterSecret ──

func TestResolveMasterSecret_FromEnv_Base64(t *testing.T) {
	env, _, _ := newTestEnv(t)
	want := make([]byte, 32)
	_, _ = rand.Read(want)
	env.getEnv = func(k string) string {
		if k == "SAGE_MASTER_SECRET" {
			return base64.StdEncoding.EncodeToString(want)
		}
		return ""
	}
	got, err := resolveMasterSecret(env)
	if err != nil {
		t.Fatalf("resolveMasterSecret: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("master secret mismatch")
	}
}

func TestResolveMasterSecret_FromFile_RawBytes(t *testing.T) {
	env, _, _ := newTestEnv(t)
	want := make([]byte, 32)
	_, _ = rand.Read(want)
	writeFile(t, filepath.Join(env.dataDir, "master.key"), want, 0600)
	got, err := resolveMasterSecret(env)
	if err != nil {
		t.Fatalf("resolveMasterSecret: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("master secret mismatch")
	}
}

func TestResolveMasterSecret_FromFile_Base64(t *testing.T) {
	env, _, _ := newTestEnv(t)
	want := make([]byte, 32)
	_, _ = rand.Read(want)
	b64 := base64.StdEncoding.EncodeToString(want)
	writeFile(t, filepath.Join(env.dataDir, "master.key"), []byte(b64), 0600)
	got, err := resolveMasterSecret(env)
	if err != nil {
		t.Fatalf("resolveMasterSecret: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("master secret mismatch")
	}
}

func TestResolveMasterSecret_Missing_ReturnsSentinel(t *testing.T) {
	env, _, _ := newTestEnv(t)
	_, err := resolveMasterSecret(env)
	if !errors.Is(err, ErrNoMasterSecret) {
		t.Fatalf("expected ErrNoMasterSecret, got %v", err)
	}
}

func TestResolveMasterSecret_EnvUnparseable_FallsBackToFile(t *testing.T) {
	env, _, _ := newTestEnv(t)
	want := make([]byte, 32)
	_, _ = rand.Read(want)
	writeFile(t, filepath.Join(env.dataDir, "master.key"), want, 0600)
	env.getEnv = func(k string) string {
		if k == "SAGE_MASTER_SECRET" {
			return "not-base64-or-hex-or-32-bytes"
		}
		return ""
	}
	got, err := resolveMasterSecret(env)
	if err != nil {
		t.Fatalf("resolveMasterSecret: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("expected fallback to file")
	}
}

// ── dispatch ──

func TestRunAuth_NoArgs_ReturnsGenericError(t *testing.T) {
	env, _, stderr := newTestEnv(t)
	code := runAuth(env, nil)
	if code != exitErrorGeneric {
		t.Errorf("exit code = %d, want %d", code, exitErrorGeneric)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("expected usage in stderr, got %q", stderr.String())
	}
}

func TestRunAuth_Help_ReturnsOK(t *testing.T) {
	env, stdout, _ := newTestEnv(t)
	code := runAuth(env, []string{"help"})
	if code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("expected usage in stdout")
	}
}

func TestRunAuth_UnknownCommand_ReturnsGenericError(t *testing.T) {
	env, _, stderr := newTestEnv(t)
	code := runAuth(env, []string{"nonsense"})
	if code != exitErrorGeneric {
		t.Errorf("exit code = %d, want %d", code, exitErrorGeneric)
	}
	if !strings.Contains(stderr.String(), "unknown auth command") {
		t.Errorf("expected unknown command message, got %q", stderr.String())
	}
}

// ── exit codes ──

func TestRunLogin_NoMasterSecret_ReturnsCode5(t *testing.T) {
	env, _, stderr := newTestEnv(t)
	code := runAuth(env, []string{"login", "--provider", "openai"})
	if code != exitNoMasterKey {
		t.Errorf("exit code = %d, want %d", code, exitNoMasterKey)
	}
	if !strings.Contains(stderr.String(), "master secret") {
		t.Errorf("expected master-secret message in stderr, got %q", stderr.String())
	}
}

func TestRunImport_NoMasterSecret_ReturnsCode5(t *testing.T) {
	env, _, _ := newTestEnv(t)
	code := runAuth(env, []string{"import", "--provider", "gemini"})
	if code != exitNoMasterKey {
		t.Errorf("exit code = %d, want %d", code, exitNoMasterKey)
	}
}

func TestRunLogin_RejectsImportOnlyProvider(t *testing.T) {
	env, _, _ := newTestEnv(t)
	// Even without a master secret, the provider check happens first.
	code := runAuth(env, []string{"login", "--provider", "gemini"})
	if code != exitErrorGeneric {
		t.Errorf("exit code = %d, want %d", code, exitErrorGeneric)
	}
}

// withMasterSecret sets up a temp dir with a master.key and returns
// the env. Used by tests that need to exercise the post-master path.
func withMasterSecret(t *testing.T) (cliEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	env, stdout, stderr := newTestEnv(t)
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	writeFile(t, filepath.Join(env.dataDir, "master.key"), mk, 0600)
	return env, stdout, stderr
}

func TestRunLogin_TOSRejected_ReturnsCode6(t *testing.T) {
	env, _, _ := withMasterSecret(t)
	env.stdin = strings.NewReader("n\n") // user types "n"
	code := runAuth(env, []string{"login", "--provider", "openai"})
	if code != exitTOSRejected {
		t.Errorf("exit code = %d, want %d", code, exitTOSRejected)
	}
}

func TestRunImport_TOSRejected_ReturnsCode6(t *testing.T) {
	env, _, _ := withMasterSecret(t)
	env.stdin = strings.NewReader("n\n")
	code := runAuth(env, []string{"import", "--provider", "openai"})
	if code != exitTOSRejected {
		t.Errorf("exit code = %d, want %d", code, exitTOSRejected)
	}
}

// ── status / logout against a seeded store ──

func seedConn(t *testing.T, db store.Store, c *store.Connection) {
	t.Helper()
	if err := db.CreateConnection(c); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// openSeededStore creates a DB in env.dataDir matching what openCLIStore
// would see, seeds it, and returns a closer.
func openSeededStore(t *testing.T, env cliEnv) store.Store {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	writeFile(t, filepath.Join(env.dataDir, "master.key"), mk, 0600)
	dbPath := filepath.Join(env.dataDir, "sage-router.db")
	db, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.SetEncryptionKey(deriveSubkey(mk, "sage-router:encryption"))
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunStatus_NoConnections_PrintsEmpty(t *testing.T) {
	env, stdout, _ := newTestEnv(t)
	_ = openSeededStore(t, env)
	code := runAuth(env, []string{"status"})
	if code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "(no connections)") {
		t.Errorf("expected empty marker, got %q", stdout.String())
	}
}

func TestRunStatus_PrintsTable(t *testing.T) {
	env, stdout, _ := newTestEnv(t)
	db := openSeededStore(t, env)
	seedConn(t, db, &store.Connection{
		ID: "c1", Provider: "openai", Name: "Work",
		AuthType: auth.AuthTypeSubscription, State: "idle",
	})
	code := runAuth(env, []string{"status"})
	if code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "openai") || !strings.Contains(out, "Work") || !strings.Contains(out, "subscription") {
		t.Errorf("missing expected fields in status output:\n%s", out)
	}
}

func TestRunLogout_DeletesByID(t *testing.T) {
	env, stdout, _ := newTestEnv(t)
	db := openSeededStore(t, env)
	seedConn(t, db, &store.Connection{
		ID: "c-test-1", Provider: "openai", Name: "Work",
		AuthType: auth.AuthTypeSubscription, State: "idle",
	})
	code := runAuth(env, []string{"logout", "--id", "c-test-1"})
	if code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Deleted connection c-test-1") {
		t.Errorf("expected delete confirmation, got %q", stdout.String())
	}
	left, _ := db.ListConnections(store.ConnectionFilter{})
	if len(left) != 0 {
		t.Errorf("expected 0 connections, got %d", len(left))
	}
}

func TestRunLogout_DeletesAllSubscriptionsForProvider(t *testing.T) {
	env, stdout, _ := newTestEnv(t)
	db := openSeededStore(t, env)
	seedConn(t, db, &store.Connection{ID: "c-sub-1", Provider: "openai", AuthType: auth.AuthTypeSubscription, State: "idle"})
	seedConn(t, db, &store.Connection{ID: "c-sub-2", Provider: "openai", AuthType: auth.AuthTypeSubscription, State: "idle"})
	// API-key row should be kept.
	seedConn(t, db, &store.Connection{ID: "c-key-1", Provider: "openai", AuthType: "apikey", APIKey: "sk-test", State: "idle"})

	code := runAuth(env, []string{"logout", "--provider", "openai"})
	if code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Deleted 2 subscription") {
		t.Errorf("expected 2-deletion message, got %q", stdout.String())
	}
	left, _ := db.ListConnections(store.ConnectionFilter{})
	if len(left) != 1 || left[0].ID != "c-key-1" {
		t.Errorf("expected only c-key-1 remaining, got %+v", left)
	}
}

func TestRunImport_FileNotFound_PrintsError(t *testing.T) {
	env, _, stderr := newTestEnv(t)
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	writeFile(t, filepath.Join(env.dataDir, "master.key"), mk, 0600)
	env.stdin = strings.NewReader("y\n") // accept TOS
	// Override the default path so the test doesn't read the real
	// ~/.codex/auth.json.
	code := runAuth(env, []string{"import", "--provider", "openai", "--path", filepath.Join(env.dataDir, "does-not-exist.json")})
	if code != exitErrorGeneric {
		t.Errorf("exit code = %d, want %d", code, exitErrorGeneric)
	}
	if !strings.Contains(stderr.String(), "auth import") {
		t.Errorf("expected error prefix, got %q", stderr.String())
	}
}

func TestSourceFromProviderData_ExtractsAccountID(t *testing.T) {
	got := sourceFromProviderData([]byte(`{"account_id":"acct-abc","extra":{"foo":"bar"}}`))
	if got != "acct-abc" {
		t.Errorf("source = %q, want acct-abc", got)
	}
}

func TestSourceFromProviderData_FallsBackToExtra(t *testing.T) {
	got := sourceFromProviderData([]byte(`{"extra":{"github_user":"ninh"}}`))
	if got != "ninh" {
		t.Errorf("source = %q, want ninh", got)
	}
}

func TestSourceFromProviderData_HandlesEmpty(t *testing.T) {
	if got := sourceFromProviderData(nil); got != "—" {
		t.Errorf("empty source = %q, want —", got)
	}
}
