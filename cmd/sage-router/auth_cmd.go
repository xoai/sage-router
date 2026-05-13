package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/browser"
	"sage-router/internal/auth/imports"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/auth/providers"
	"sage-router/internal/auth/refresh"
	"sage-router/internal/config"
	"sage-router/internal/store"
)

// CLI exit codes (spec AC34/AC44e + decisions).
const (
	exitOK            = 0
	exitErrorGeneric  = 1
	exitPortBusy      = 4
	exitNoMasterKey   = 5
	exitTOSRejected   = 6
)

const cliUsage = `Usage: sage-router auth <command> [flags]

Commands:
  login    --provider <p> [--name N] [--priority P]
  import   --provider <p> [--name N] [--priority P] [--path PATH]
  status   [--provider <p>]
  logout   --provider <p> [--id <id>]

Providers: openai, anthropic, gemini, github-copilot

Exit codes: 0 ok | 4 port busy | 5 no master secret | 6 TOS rejected
`

// runAuthCommand is the entry point from main(). Returns the process
// exit code. Stdin/stdout/stderr are the real ones in production; tests
// substitute these via cliEnv to keep things deterministic.
func runAuthCommand(args []string) int {
	return runAuth(defaultEnv(), args)
}

// cliEnv collects the moving parts so test cases can inject in-memory
// alternatives without subprocess gymnastics.
type cliEnv struct {
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	dataDir     string                                  // overrides config.DefaultDataDir() when set
	getEnv      func(string) string                     // env-var lookup
	openBrowser func(string) error                      // os.exec.Command wrapper
	newBridge   func(oauth.BridgeHandler) *oauth.Bridge // production: oauth.NewBridge
	probeServer func() (running bool, address string)   // probes /api/auth/check
	now         func() time.Time
}

func defaultEnv() cliEnv {
	return cliEnv{
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		dataDir:     "",
		getEnv:      os.Getenv,
		openBrowser: browser.Open,
		newBridge:   oauth.NewBridge,
		probeServer: probeRunningServer,
		now:         time.Now,
	}
}

// probeRunningServer attempts a fast localhost probe of the dashboard
// API. Returns true if a sage-router is responding on the default port.
// This is informational only — the CLI continues with the offline path
// either way, but warns the user that the dashboard route is available
// and that bridge ports will collide.
func probeRunningServer() (bool, string) {
	addr := fmt.Sprintf("%s:%d", config.DefaultHost, config.DefaultPort)
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/auth/check")
	if err != nil {
		return false, ""
	}
	resp.Body.Close()
	return true, addr
}

func (e cliEnv) resolveDataDir() string {
	if e.dataDir != "" {
		return e.dataDir
	}
	return config.DefaultDataDir()
}

func runAuth(env cliEnv, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(env.stderr, cliUsage)
		return exitErrorGeneric
	}
	switch args[0] {
	case "login":
		return runLogin(env, args[1:])
	case "import":
		return runImport(env, args[1:])
	case "status":
		return runStatus(env, args[1:])
	case "logout":
		return runLogout(env, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(env.stdout, cliUsage)
		return exitOK
	default:
		fmt.Fprintf(env.stderr, "unknown auth command: %q\n\n%s", args[0], cliUsage)
		return exitErrorGeneric
	}
}

// ── Master secret resolution ──

// resolveMasterSecret returns the 32-byte master key, looking first at
// SAGE_MASTER_SECRET (base64 or hex), then ~/.sage-router/master.key
// (raw bytes or base64). No fallback to the DB — that would mean writing
// plaintext tokens before the encryption key is wired (C3 in the design
// review). Returns ErrNoMasterSecret if neither source has a usable key.
var ErrNoMasterSecret = errors.New("master secret not available — set SAGE_MASTER_SECRET or place a 32-byte key at ~/.sage-router/master.key")

func resolveMasterSecret(env cliEnv) ([]byte, error) {
	if v := strings.TrimSpace(env.getEnv("SAGE_MASTER_SECRET")); v != "" {
		if decoded, err := base64.StdEncoding.DecodeString(v); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
		if decoded, err := hex.DecodeString(v); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
		// Fallthrough: env was set but unparseable; try the file before
		// giving up so a user with both can still succeed via the file.
	}
	path := filepath.Join(env.resolveDataDir(), "master.key")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrNoMasterSecret
	}
	if len(data) == 32 {
		return data, nil
	}
	// Accept base64 form too — tooling that pipes the env value into the
	// file would have it base64-encoded.
	trimmed := strings.TrimSpace(string(data))
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	return nil, fmt.Errorf("master.key at %s has invalid length/format", path)
}

// openCLIStore opens the local SQLite database, derives the encryption
// subkey from the master secret, and runs pending migrations. Caller
// must Close() it.
func openCLIStore(env cliEnv) (store.Store, []byte, error) {
	master, err := resolveMasterSecret(env)
	if err != nil {
		return nil, nil, err
	}
	dbPath := filepath.Join(env.resolveDataDir(), "sage-router.db")
	db, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	if err := db.Migrate(); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	encryptionKey := deriveSubkey(master, "sage-router:encryption")
	db.SetEncryptionKey(encryptionKey)
	return db, master, nil
}

// ── TOS confirmation ──

// confirmTOS shows the TOS and reads a y/N from stdin. Idempotent (no-op
// when already acknowledged). Returns true if the user accepted (or had
// already accepted previously).
func confirmTOS(env cliEnv, db store.Store) bool {
	if auth.IsTOSAcknowledged(db) {
		return true
	}
	fmt.Fprintln(env.stdout, auth.TOSText)
	fmt.Fprint(env.stdout, "Accept? [y/N]: ")
	br := bufio.NewReader(env.stdin)
	line, _ := br.ReadString('\n')
	resp := strings.ToLower(strings.TrimSpace(line))
	if resp != "y" && resp != "yes" {
		return false
	}
	if err := auth.AcknowledgeTOS(db); err != nil {
		fmt.Fprintln(env.stderr, "could not record acceptance:", err)
		return false
	}
	return true
}

// ── login ──

func runLogin(env cliEnv, args []string) int {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	providerArg := fs.String("provider", "", "provider id (openai | anthropic)")
	connName := fs.String("name", "", "label for the connection")
	priority := fs.Int("priority", 0, "selection priority (higher = preferred)")
	if err := fs.Parse(args); err != nil {
		return exitErrorGeneric
	}
	if *providerArg == "" {
		fmt.Fprintln(env.stderr, "auth login: --provider is required")
		return exitErrorGeneric
	}
	canonical, err := providers.Resolve(*providerArg)
	if err != nil {
		fmt.Fprintln(env.stderr, "auth login:", err)
		return exitErrorGeneric
	}
	if canonical != "openai" && canonical != "anthropic" {
		fmt.Fprintf(env.stderr, "auth login: provider %q is import-only — use `auth import --provider %s` instead\n", canonical, canonical)
		return exitErrorGeneric
	}

	db, _, err := openCLIStore(env)
	if err != nil {
		if errors.Is(err, ErrNoMasterSecret) {
			fmt.Fprintln(env.stderr, err)
			return exitNoMasterKey
		}
		fmt.Fprintln(env.stderr, "auth login:", err)
		return exitErrorGeneric
	}
	defer db.Close()

	// Running-server detection (plan Task 3.6 "Running-server detection").
	// We don't have a JWT to hand off to the server's OAuth bridge from
	// here, so a running sage-router is informational: it explains why
	// the local bridge bind below is likely to fail and points at the
	// dashboard path that does have auth.
	if env.probeServer != nil {
		if running, addr := env.probeServer(); running {
			fmt.Fprintf(env.stdout,
				"Note: a sage-router is already running at %s. The CLI uses direct-SQLite mode;\n"+
					"to use the running server's OAuth bridge, open the dashboard at http://%s/dashboard.\n\n",
				addr, addr)
		}
	}

	if !confirmTOS(env, db) {
		fmt.Fprintln(env.stderr, "auth login: TOS not accepted, aborting")
		return exitTOSRejected
	}

	// Start a local bridge — same ports as the running-server case so a
	// CLI flow re-uses the listener identity the provider's redirect URI
	// expects.
	handler := &cliBridgeHandler{db: db}
	bridge := env.newBridge(handler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx); err != nil {
		fmt.Fprintln(env.stderr, "auth login: bridge start failed:", err)
		return exitErrorGeneric
	}
	defer bridge.Stop(context.Background())

	if h := bridge.Health(canonical); h != oauth.HealthAvailable {
		fmt.Fprintf(env.stderr, "auth login: bridge port for %s is in use (likely the %s CLI or a running sage-router). Stop it and retry.\n",
			canonical, friendlyCliName(canonical))
		return exitPortBusy
	}

	flow, err := oauth.NewFlow(canonical, *connName)
	if err != nil {
		fmt.Fprintln(env.stderr, "auth login: NewFlow:", err)
		return exitErrorGeneric
	}
	flow.Priority = *priority
	bridge.Register(flow)
	authorizeURL := flow.AuthorizeURL(bridge.CallbackURL(canonical))

	if err := env.openBrowser(authorizeURL); err != nil {
		fmt.Fprintln(env.stdout, "Could not auto-open browser. Open this URL manually:")
	} else {
		fmt.Fprintln(env.stdout, "Opening your browser to complete login. If it doesn't open, use this URL:")
	}
	fmt.Fprintln(env.stdout, "  ", authorizeURL)
	fmt.Fprintln(env.stdout)

	// Two completion paths: bridge callback (normal flow) or paste of
	// the redirect URL (AC49 — cross-machine / sandboxed-browser case).
	// The paste prompt only appears after a short delay AND only if the
	// callback hasn't already won. This avoids the prompt racing with
	// the "Connected." message in the common case.
	done := make(chan loginResult, 2)
	callbackWon := make(chan struct{})
	go func() {
		waitForCallback(bridge, flow.State, done)
		close(callbackWon)
	}()
	go func() {
		select {
		case <-time.After(4 * time.Second):
		case <-callbackWon:
			return // callback finished first; suppress paste prompt entirely
		}
		promptForPaste(env, flow, bridge, db, canonical, done)
	}()

	timeout := time.NewTimer(15 * time.Minute)
	defer timeout.Stop()
	for {
		select {
		case r := <-done:
			if r.err != nil {
				fmt.Fprintln(env.stderr, "auth login:", r.err)
				return exitErrorGeneric
			}
			fmt.Fprintf(env.stdout, "Connected. id=%s provider=%s\n", r.connID, canonical)
			return exitOK
		case <-timeout.C:
			fmt.Fprintln(env.stderr, "auth login: timed out waiting for the callback")
			return exitErrorGeneric
		}
	}
}

type loginResult struct {
	connID string
	err    error
}

// waitForCallback polls the bridge's Status for the given state until
// it reports completed or error. This is the happy path when the
// browser ends up at the local-bridge redirect URI.
func waitForCallback(bridge *oauth.Bridge, state string, done chan<- loginResult) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		info := bridge.Status(state)
		switch info.Status {
		case oauth.StatusComplete:
			done <- loginResult{connID: info.ConnectionID}
			return
		case oauth.StatusError:
			done <- loginResult{err: errors.New(info.Error)}
			return
		}
	}
}

// promptForPaste runs the AC49 paste-redirect fallback in a goroutine
// so the user can either let the bridge callback work OR paste the URL
// from another machine. The first to succeed wins.
//
// Note: stdin reads block until the user types something or EOF, so on
// callback-success this goroutine may sit blocked until the process
// exits. That's acceptable for a short-lived CLI — there's no resource
// the OS won't reclaim on exit. We avoid making it worse by gating the
// caller behind a 4s delay (see runLogin) so the prompt only appears
// when the user actually needs it.
func promptForPaste(env cliEnv, flow *oauth.Flow, bridge *oauth.Bridge, db store.Store, canonical string, done chan<- loginResult) {
	// Only offer the paste path when stdin looks interactive — non-TTY
	// callers (CI, scripts) don't see the prompt and rely on the
	// callback path instead.
	if env.stdin == nil {
		return
	}
	fmt.Fprintln(env.stdout, "If the browser is on another machine, paste the full redirect URL here:")
	code, state, err := browser.PromptPasteRedirect(env.stdin, env.stdout, "")
	if err != nil {
		// Don't error the whole login — the callback path may still
		// succeed. Silently swallow ParsePastedRedirect errors.
		return
	}
	if state != flow.State {
		done <- loginResult{err: fmt.Errorf("pasted URL state mismatch (got %q, want %q)", safePrefix(state), safePrefix(flow.State))}
		return
	}
	consumed, ok := bridge.Consume(state)
	if !ok || consumed == nil {
		done <- loginResult{err: errors.New("flow already consumed or expired")}
		return
	}
	resp, err := consumed.Exchange(context.Background(), code, bridge.CallbackURL(canonical))
	if err != nil {
		done <- loginResult{err: fmt.Errorf("token exchange: %w", err)}
		return
	}
	cred := consumed.ToCredential(resp)
	connID, err := writeConnection(db, consumed.Provider, consumed.ConnName, consumed.Priority, cred)
	if err != nil {
		done <- loginResult{err: err}
		return
	}
	done <- loginResult{connID: connID}
}

// safePrefix returns up to the first 8 chars of s — used for diagnostic
// output that should never leak the full state token.
func safePrefix(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}

// ── import ──

func runImport(env cliEnv, args []string) int {
	fs := flag.NewFlagSet("auth import", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	providerArg := fs.String("provider", "", "provider id")
	connName := fs.String("name", "", "label for the connection")
	priority := fs.Int("priority", 0, "selection priority")
	path := fs.String("path", "", "override the default CLI credential path")
	if err := fs.Parse(args); err != nil {
		return exitErrorGeneric
	}
	if *providerArg == "" {
		fmt.Fprintln(env.stderr, "auth import: --provider is required")
		return exitErrorGeneric
	}
	canonical, err := providers.Resolve(*providerArg)
	if err != nil {
		fmt.Fprintln(env.stderr, "auth import:", err)
		return exitErrorGeneric
	}

	db, _, err := openCLIStore(env)
	if err != nil {
		if errors.Is(err, ErrNoMasterSecret) {
			fmt.Fprintln(env.stderr, err)
			return exitNoMasterKey
		}
		fmt.Fprintln(env.stderr, "auth import:", err)
		return exitErrorGeneric
	}
	defer db.Close()

	if !confirmTOS(env, db) {
		fmt.Fprintln(env.stderr, "auth import: TOS not accepted, aborting")
		return exitTOSRejected
	}

	cred, err := imports.ImportFromCLI(canonical, *path)
	if err != nil {
		if errors.Is(err, imports.ErrFileNotFound) {
			fmt.Fprintln(env.stderr, "auth import:", err)
			if canonical == "anthropic" && imports.MacOSClaudeKeychainHint(err) {
				fmt.Fprintln(env.stderr, imports.KeychainHintMessage)
			}
			return exitErrorGeneric
		}
		fmt.Fprintln(env.stderr, "auth import:", err)
		return exitErrorGeneric
	}

	// Copilot import returns an empty AccessToken — the long-lived GH
	// OAuth token is in RefreshToken, we mint a real bearer via 2-step
	// refresh before persisting.
	if canonical == "github-copilot" && cred.AccessToken == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fresh, err := refresh.Refresh(ctx, cred)
		if err != nil {
			fmt.Fprintln(env.stderr, "auth import: imported but initial refresh failed:", err)
			return exitErrorGeneric
		}
		cred = fresh
		cred.Provider = canonical
	}

	id, err := writeConnection(db, canonical, *connName, *priority, cred)
	if err != nil {
		fmt.Fprintln(env.stderr, "auth import:", err)
		return exitErrorGeneric
	}
	fmt.Fprintf(env.stdout, "Imported. id=%s provider=%s\n", id, canonical)
	return exitOK
}

// ── status ──

func runStatus(env cliEnv, args []string) int {
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	providerArg := fs.String("provider", "", "filter by provider")
	if err := fs.Parse(args); err != nil {
		return exitErrorGeneric
	}

	db, _, err := openCLIStore(env)
	if err != nil {
		if errors.Is(err, ErrNoMasterSecret) {
			fmt.Fprintln(env.stderr, err)
			return exitNoMasterKey
		}
		fmt.Fprintln(env.stderr, "auth status:", err)
		return exitErrorGeneric
	}
	defer db.Close()

	filter := store.ConnectionFilter{}
	if *providerArg != "" {
		canonical, err := providers.Resolve(*providerArg)
		if err != nil {
			fmt.Fprintln(env.stderr, "auth status:", err)
			return exitErrorGeneric
		}
		filter.Provider = canonical
	}
	conns, err := db.ListConnections(filter)
	if err != nil {
		fmt.Fprintln(env.stderr, "auth status:", err)
		return exitErrorGeneric
	}

	tw := tabwriter.NewWriter(env.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROVIDER\tNAME\tAUTH\tTOKEN\tSOURCE\tEXPIRES\tSTATE\tFAILS")
	for _, c := range conns {
		source := "—"
		expires := "—"
		switch c.AuthType {
		case auth.AuthTypeSubscription, "auto_detect":
			source = sourceFromProviderData(c.ProviderData)
		}
		if c.ExpiresAt != nil {
			expires = c.ExpiresAt.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			c.ID, c.Provider, c.Name, c.AuthType, maskToken(c), source, expires, c.State, c.RefreshFailures)
	}
	tw.Flush()
	if len(conns) == 0 {
		fmt.Fprintln(env.stdout, "(no connections)")
	}
	return exitOK
}

// maskToken returns a printable preview of a connection's primary
// secret — the first 4 chars of AccessToken/APIKey when available, the
// last 4 of RefreshToken when only the refresh side is set (Copilot
// imports look like this between import and first refresh). Never
// returns more than 6 characters of any one secret, and dashes out the
// case where no secret material exists at all.
func maskToken(c store.Connection) string {
	pick := func(s string) string {
		if len(s) <= 4 {
			return s
		}
		return s[:4] + "…"
	}
	switch {
	case c.AccessToken != "":
		return pick(c.AccessToken)
	case c.APIKey != "":
		return pick(c.APIKey)
	case c.RefreshToken != "":
		return pick(c.RefreshToken)
	}
	return "—"
}

func sourceFromProviderData(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "—"
	}
	var pd struct {
		AccountID string         `json:"account_id"`
		Extra     map[string]any `json:"extra"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		return "—"
	}
	if pd.AccountID != "" {
		return pd.AccountID
	}
	if pd.Extra != nil {
		// Surface the most distinguishing field if present.
		for _, k := range []string{"github_user", "subscription_type", "scope"} {
			if v, ok := pd.Extra[k]; ok {
				return fmt.Sprintf("%v", v)
			}
		}
	}
	return "—"
}

// ── logout ──

func runLogout(env cliEnv, args []string) int {
	fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	providerArg := fs.String("provider", "", "provider id")
	idArg := fs.String("id", "", "specific connection id to delete (default: all subscription rows for the provider)")
	if err := fs.Parse(args); err != nil {
		return exitErrorGeneric
	}
	if *providerArg == "" && *idArg == "" {
		fmt.Fprintln(env.stderr, "auth logout: --provider or --id is required")
		return exitErrorGeneric
	}

	db, _, err := openCLIStore(env)
	if err != nil {
		if errors.Is(err, ErrNoMasterSecret) {
			fmt.Fprintln(env.stderr, err)
			return exitNoMasterKey
		}
		fmt.Fprintln(env.stderr, "auth logout:", err)
		return exitErrorGeneric
	}
	defer db.Close()

	if *idArg != "" {
		if err := db.DeleteConnection(*idArg); err != nil {
			fmt.Fprintln(env.stderr, "auth logout:", err)
			return exitErrorGeneric
		}
		fmt.Fprintf(env.stdout, "Deleted connection %s\n", *idArg)
		return exitOK
	}

	canonical, err := providers.Resolve(*providerArg)
	if err != nil {
		fmt.Fprintln(env.stderr, "auth logout:", err)
		return exitErrorGeneric
	}
	conns, err := db.ListConnections(store.ConnectionFilter{Provider: canonical})
	if err != nil {
		fmt.Fprintln(env.stderr, "auth logout:", err)
		return exitErrorGeneric
	}
	deleted := 0
	for _, c := range conns {
		if c.AuthType != auth.AuthTypeSubscription && c.AuthType != "auto_detect" {
			continue
		}
		if err := db.DeleteConnection(c.ID); err != nil {
			fmt.Fprintln(env.stderr, "auth logout: delete", c.ID+":", err)
			continue
		}
		deleted++
	}
	fmt.Fprintf(env.stdout, "Deleted %d subscription connection(s) for %s\n", deleted, canonical)
	return exitOK
}

// ── shared helpers ──

// cliBridgeHandler implements oauth.BridgeHandler. The CLI variant
// writes Connection rows directly to SQLite (no in-memory selector
// because we're a short-lived process). Mirrors server-side
// createSubscriptionConnection.
type cliBridgeHandler struct {
	db store.Store
}

func (h *cliBridgeHandler) OnFlowComplete(_ context.Context, flow *oauth.Flow, resp *oauth.TokenResponse) (string, string, error) {
	cred := flow.ToCredential(resp)
	id, err := writeConnection(h.db, flow.Provider, flow.ConnName, flow.Priority, cred)
	if err != nil {
		return "", "", err
	}
	// No redirect-back for CLI flows — the bridge will render its
	// generic completion page.
	return id, "", nil
}

func writeConnection(db store.Store, providerID, connName string, priority int, cred *auth.Credential) (string, error) {
	if connName == "" {
		connName = providerID
	}
	id := newCLIConnectionID()
	conn := &store.Connection{
		ID:           id,
		Provider:     providerID,
		Name:         connName,
		AuthType:     auth.AuthTypeSubscription,
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		Priority:     priority,
		State:        "idle",
	}
	if !cred.ExpiresAt.IsZero() {
		t := cred.ExpiresAt
		conn.ExpiresAt = &t
	}
	if cred.AccountID != "" || len(cred.ExtraData) > 0 {
		pd := struct {
			AccountID string         `json:"account_id,omitempty"`
			Extra     map[string]any `json:"extra,omitempty"`
		}{AccountID: cred.AccountID, Extra: cred.ExtraData}
		raw, _ := json.Marshal(pd)
		conn.ProviderData = raw
	}
	if err := db.CreateConnection(conn); err != nil {
		return "", fmt.Errorf("create connection: %w", err)
	}
	return id, nil
}

func newCLIConnectionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback uses nanosecond clock — non-cryptographic but unique
		// enough for a CLI run and very unlikely path.
		return fmt.Sprintf("c-cli-%d", time.Now().UnixNano())
	}
	return "c-cli-" + hex.EncodeToString(b[:])
}

func friendlyCliName(canonical string) string {
	switch canonical {
	case "openai":
		return "Codex"
	case "anthropic":
		return "Claude"
	}
	return canonical
}
