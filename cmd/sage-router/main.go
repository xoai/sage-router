package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/auth/refresh"
	"sage-router/internal/bypass"
	"sage-router/internal/catalog"
	"sage-router/internal/config"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/ratelimit"
	"sage-router/internal/routing"
	"sage-router/internal/server"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
	geminiTranslate "sage-router/internal/translate/gemini"
	openaiTranslate "sage-router/internal/translate/openai"
	openaiRespTranslate "sage-router/internal/translate/openai-responses"
	"sage-router/internal/usage"
	"sage-router/web"
)

var version = "dev"

func main() {
	// Dispatch subcommands before the global flag parse. `auth` and its
	// children own their own flag sets — letting the global parser at
	// them would steal --provider as `host`.
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		os.Exit(runAuthCommand(os.Args[2:]))
	}

	// Parse flags
	host := flag.String("host", config.DefaultHost, "Listen host")
	port := flag.Int("port", config.DefaultPort, "Listen port")
	dbPath := flag.String("db", "", "Database path (default: ~/.sage-router/sage-router.db)")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("sage-router %s\n", version)
		os.Exit(0)
	}

	// Setup logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Determine data directory
	dataDir := config.DefaultDataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		slog.Error("failed to create data directory", "path", dataDir, "error", err)
		os.Exit(1)
	}

	// Database path
	if *dbPath == "" {
		*dbPath = filepath.Join(dataDir, "sage-router.db")
	}

	// Initialize store (encryption key added after bootstrap)
	db, err := store.NewSQLiteStore(*dbPath)
	if err != nil {
		slog.Error("failed to open database", "path", *dbPath, "error", err)
		os.Exit(1)
	}

	if err := db.Migrate(); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	// Models Discovery M1.11 + M2 — wire the runtime catalog (Registry,
	// raw Store for writes, and DiscoveryRunner). Seeds catalog tables
	// from the static config constants on first boot; idempotent on
	// subsequent boots. Also ensures settings.openrouter_refresh_enabled
	// exists for M2's OpenRouter refresher to read.
	catalogW, err := wireCatalog(context.Background(), db.DB())
	if err != nil {
		slog.Error("failed to wire catalog", "error", err)
		os.Exit(1)
	}

	// Bootstrap: master secret + password
	masterSecret, passwordHash := bootstrap(db)

	// Derive subkeys from master secret
	jwtSecret := deriveSubkey(masterSecret, "sage-router:jwt")
	hmacSecret := deriveSubkey(masterSecret, "sage-router:hmac")
	encryptionKey := deriveSubkey(masterSecret, "sage-router:encryption")

	// Enable credential encryption on the store
	db.SetEncryptionKey(encryptionKey)

	// Initialize auth manager
	authMgr := auth.NewManager(passwordHash, jwtSecret, hmacSecret)

	// Initialize translate registry
	translateReg := translate.NewRegistry()
	translateReg.Register(openaiTranslate.New())
	translateReg.Register(claudeTranslate.New())
	translateReg.Register(geminiTranslate.New())
	translateReg.Register(openaiRespTranslate.New())

	// Initialize provider registry and selector
	providerReg := provider.NewRegistry()
	for id, p := range config.KnownProviders {
		providerReg.Register(id, provider.ProviderMeta{
			ID:        p.ID,
			Name:      p.Name,
			Format:    p.Format,
			BaseURL:   p.BaseURL,
			AuthTypes: p.AuthTypes,
		})
	}

	providerSel := provider.NewSelector()

	// Load connections from store into selector
	connections, err := db.ListConnections(store.ConnectionFilter{})
	if err == nil {
		for i := range connections {
			c := &connections[i]
			conn := provider.NewConnection(c.ID, c.Provider, c.Name, c.Priority, c.AuthType)
			// Hydrate the Lifecycle facet from the persisted state: a
			// connection an operator disabled must stay disabled across a
			// restart. The breaker / transient-health facets are deliberately
			// not persisted (they load CLOSED); only Disabled carries over.
			// Without this, a persisted-disabled connection would load
			// Idle/selectable (M2 spec §10).
			if c.State == "disabled" {
				if derr := conn.Disable(); derr != nil {
					slog.Warn("hydrate disabled connection failed",
						"conn_id", c.ID, "err", derr)
				}
			}
			providerSel.Register(conn)
		}
	}

	// Initialize executors
	clientPool := executor.NewClientPool()
	executors := map[string]executor.Executor{
		// Default-executor baseURLs are API ROOTS (per default.go:22 contract).
		// Bug-fix 20260515-chat-routing-fix AC-B1: previously these strings
		// included `/chat/completions`, which doubled with the executor's
		// default endpoint append → e.g., `…/v1/chat/completions/chat/completions`.
		"openai":         executor.NewDefaultExecutor("openai", "https://api.openai.com/v1", clientPool),
		"anthropic":      executor.NewClaudeExecutor("https://api.anthropic.com", clientPool),
		"gemini":         executor.NewGeminiExecutor("https://generativelanguage.googleapis.com/v1beta", clientPool),
		"github-copilot": executor.NewGitHubCopilotExecutor("https://api.githubcopilot.com", clientPool),
		"openrouter":     executor.NewDefaultExecutor("openrouter", "https://openrouter.ai/api/v1", clientPool),
		"ollama":         executor.NewDefaultExecutor("ollama", "http://localhost:11434/v1", clientPool),
		"default":        executor.NewDefaultExecutor("default", "", clientPool),
	}

	// Wrap executors with retry logic
	retryCfg := executor.DefaultRetryConfig()
	for id, exec := range executors {
		executors[id] = executor.NewRetryExecutor(exec, retryCfg)
	}

	// Build the variant dispatch registry alongside the legacy provider-keyed
	// map. M1.7 of cycle 20260517-provider-auth-variants. Today every entry
	// from the legacy map is registered as a wildcard `(provider, "")`;
	// M2.8a registers (openai, subscription) → CodexSubscriptionExecutor
	// as the first explicit (P, A) entry. M3 will add (anthropic, subscription)
	// → ClaudeMaxExecutor.
	//
	// Variants and Executors coexist during the migration; M5.6 drops the
	// legacy map once every call site has migrated to Variants.Get. The
	// route handler's resolveVariantExec already prefers Variants when set.
	variants := executor.NewVariants()
	for id, exec := range executors {
		variants.Register(executor.VariantKey{Provider: id, AuthType: ""}, exec)
	}
	// M2.8a (cycle 20260517-provider-auth-variants):
	// Register CodexSubscriptionExecutor for (openai, subscription). The
	// variant routes to chatgpt.com/backend-api/codex/responses with the
	// 5 required headers + uses the PKCE access_token directly (no RFC 8693
	// exchange). RetryExecutor wrapping is mandatory per memory `fb0b4ef62`
	// — variant optional-interface methods (Format, ParseAuthError,
	// PreflightCredentials) are forwarded through the wrap.
	codexSubExec := executor.NewCodexSubscriptionExecutor(clientPool)
	variants.Register(
		executor.VariantKey{Provider: "openai", AuthType: "subscription"},
		executor.NewRetryExecutor(codexSubExec, retryCfg),
	)

	// Initialize usage tracker
	usageTracker := usage.NewTracker(db)

	// Setup dashboard filesystem
	var dashboardFS fs.FS
	sub, err := fs.Sub(web.DashboardFS, "dashboard/dist")
	if err == nil {
		dashboardFS = sub
	}

	// Generate setup token if no password set (first run)
	var setupToken string
	if authMgr.NeedsSetup() {
		setupToken = authMgr.GenerateSetupToken()
	}

	// AuthStore — wraps the DB with the auth package's narrow Credential
	// interface. The OAuth handlers and the refresh loop both go through
	// this so the same encryption and provider_data shape is used end-to-end.
	authStore := auth.NewAuthStore(newAuthStoreAdapter(db))

	// Create and start server
	srv := server.New(server.Config{
		Host:        *host,
		Port:        *port,
		DBPath:      *dbPath,
		DashboardFS: dashboardFS,
		SetupToken:  setupToken,
	}, server.Dependencies{
		Store:             db,
		Catalog:           catalogW.Registry,
		CatalogStore:      catalogW.Store,
		Discovery:         catalogW.Discovery,
		TranslateRegistry: translateReg,
		ProviderSelector:  providerSel,
		ProviderRegistry:  providerReg,
		// Cycle 20260517-provider-auth-variants M5.6: legacy Executors
		// map removed from server.Dependencies; Variants is the sole
		// dispatch source. The `executors` local var above is still
		// used to wrap each entry in RetryExecutor + register into Variants.
		Variants:          variants,
		UsageTracker:      usageTracker,
		Auth:              authMgr,
		OpenAIAuth:        oauth.NewOpenAIAuth(),
		AuthStore:         authStore,
		SmartRouter:       routing.NewSmartRouter(),
		ConversationStore: routing.NewConversationStore(),
		BypassFilter:      bypass.NewFilter(),
		HealthChecker:     provider.NewHealthChecker(providerSel, 60*time.Second),
		RateLimiter:       ratelimit.New(),
	})

	// OAuthBridge — needs the server back-reference to create Connection
	// rows on PKCE flow completion, so wired post-server-construction.
	// Bridge binds the spec-fixed ports 1455 (openai) and 53692
	// (anthropic); if either is already held by Codex/Claude Code, the
	// bridge enters degraded mode for that provider but the rest of the
	// server runs normally (AC44b).
	bridge := srv.InitOAuthBridge()
	bridgeCtx, cancelBridge := context.WithCancel(context.Background())
	defer cancelBridge()
	if err := bridge.Start(bridgeCtx); err != nil {
		slog.Warn("oauth bridge start failed", "error", err)
		// Don't abort — the rest of the server is still usable.
	}
	defer bridge.Stop(context.Background())

	// Refresh loop — proactively refreshes subscription tokens that
	// are nearing expiry and retries refreshes for connections sitting
	// in AuthExpired. Runs for the lifetime of the process.
	refreshLoop := refresh.NewLoop(authStore, refresh.Refresh, providerSel.SnapshotAll)
	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	defer cancelRefresh()
	go refreshLoop.Run(refreshCtx)

	// Models Discovery M2.8 — OpenRouter pricing refresher. Opt-in via
	// `settings.openrouter_refresh_enabled` (seeded `true` by
	// wireCatalog/M1.11; user can flip to `false` to disable).
	// Initial refresh after a 30s settle so the listener is up first;
	// every 24h thereafter. Failures are logged + non-fatal — the
	// catalog falls back to whatever rows already exist (seed +
	// per-provider discovery).
	//
	// Restart requirement: this setting is read ONCE at boot. A runtime
	// flip via the dashboard (PUT /api/settings/openrouter_refresh_enabled)
	// persists to the DB but does not start/stop the refresher until the
	// process restarts. A live-toggle would need a settings-hot-reload
	// subsystem that does not exist yet; until then the dashboard tooltip
	// at web/dashboard/src/pages/providers.jsx surfaces this to operators.
	settingValue, settingErr := db.GetSetting("openrouter_refresh_enabled")
	switch {
	case errors.Is(settingErr, store.ErrSettingNotFound):
		// Missing setting row — fail-closed (refresher off). Bootstrap
		// (wireCatalog/M1.11) seeds the row to "true" on first boot, so a
		// missing row indicates a non-bootstrapped or manually-edited DB.
		// Surface explicitly so the operator can spot the misconfiguration.
		//
		// ORDER MATTERS: this arm MUST precede the generic settingErr != nil
		// arm because the wrapped not-found error is also non-nil, and
		// Go switch evaluates top-to-bottom with first-match-wins.
		slog.Warn("openrouter_refresh_enabled setting absent; refresher disabled this boot",
			"hint", "wireCatalog seeds this on first boot; re-run bootstrap if expected")
	case settingErr != nil:
		slog.Warn("openrouter_refresh_enabled lookup failed; refresher disabled this boot",
			"err", settingErr)
	case settingValue == "true":
		(&catalog.OpenRouterRefresher{
			Store:    catalogW.Store,
			Registry: catalogW.Registry,
		}).Start(refreshCtx, 30*time.Second, 24*time.Hour)
	}

	// Models Discovery M2.5 — 24h background discovery ticker (AC15).
	// Sweeps every provider with DiscoveryEnabled=true and no active
	// backoff, calls the matching ModelLister via DiscoveryRunner.
	// buildCredsLookup is extracted to credslookup.go so the adapter
	// has direct unit-test coverage independent of the goroutine
	// catalog.StartBackgroundRefresh spawns (M3 polish item #34).
	catalog.StartBackgroundRefresh(refreshCtx, catalogW.Discovery, buildCredsLookup(db, authStore))

	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func bootstrap(db store.Store) (masterSecret []byte, passwordHash string) {
	// 1. Load or generate master secret
	masterB64, err := db.GetSetting("master_secret")
	if err != nil || masterB64 == "" {
		masterSecret = make([]byte, 32)
		if _, err := rand.Read(masterSecret); err != nil {
			slog.Error("failed to generate master secret", "error", err)
			os.Exit(1)
		}
		db.SetSetting("master_secret", base64.StdEncoding.EncodeToString(masterSecret))
	} else {
		masterSecret, err = base64.StdEncoding.DecodeString(masterB64)
		if err != nil || len(masterSecret) != 32 {
			slog.Error("corrupt master secret in database")
			os.Exit(1)
		}
	}

	// Persist master secret to ~/.sage-router/master.key (mode 0600) so
	// the CLI's offline path (AC34) has a source independent of the DB.
	// The file mirrors the DB setting — DB remains authoritative for the
	// server. Failures here are non-fatal because the server itself can
	// continue using the DB-stored value.
	persistMasterKeyFile(masterSecret)

	// 2. Load password hash (may be empty on first run — setup happens via dashboard)
	passwordHash, _ = db.GetSetting("password_hash")

	// 3. Clear legacy plaintext password if it exists
	if legacy, _ := db.GetSetting("password"); legacy != "" {
		db.SetSetting("password", "")
	}

	return masterSecret, passwordHash
}

func deriveSubkey(master []byte, domain string) []byte {
	h := sha256.New()
	h.Write(master)
	h.Write([]byte(domain))
	return h.Sum(nil)
}

// persistMasterKeyFile writes the 32-byte master secret to
// ~/.sage-router/master.key (mode 0600) so the CLI can resolve it
// offline. The file content is the raw 32 bytes — base64 form is
// recognised at read time but not written, to keep the file small and
// to match the env-var convention (SAGE_MASTER_SECRET accepts base64
// because env vars must be text). Failures are logged, not fatal.
func persistMasterKeyFile(masterSecret []byte) {
	path := filepath.Join(config.DefaultDataDir(), "master.key")
	if existing, err := os.ReadFile(path); err == nil {
		// Already present — overwrite only if mismatched, otherwise
		// leave the file's mtime alone for human auditability.
		if len(existing) == 32 || len(existing) == 44 /* base64 */ {
			decoded, _ := base64.StdEncoding.DecodeString(string(existing))
			if len(existing) == 32 && bytesEqual(existing, masterSecret) {
				return
			}
			if len(decoded) == 32 && bytesEqual(decoded, masterSecret) {
				return
			}
		}
	}
	if err := os.WriteFile(path, masterSecret, 0600); err != nil {
		slog.Warn("could not persist master.key for CLI", "path", path, "error", err)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
