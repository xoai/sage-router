package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/bypass"
	"sage-router/internal/config"
	"sage-router/internal/routing"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/ratelimit"
	"sage-router/internal/server"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
	geminiTranslate "sage-router/internal/translate/gemini"
	openaiTranslate "sage-router/internal/translate/openai"
	"sage-router/internal/usage"
	"sage-router/web"
)

var version = "dev"

func main() {
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
			providerSel.Register(provider.NewConnection(c.ID, c.Provider, c.Name, c.Priority, c.AuthType))
		}
	}

	// Initialize executors
	clientPool := executor.NewClientPool()
	executors := map[string]executor.Executor{
		"openai":         executor.NewDefaultExecutor("openai", "https://api.openai.com/v1/chat/completions", clientPool),
		"anthropic":      executor.NewClaudeExecutor("https://api.anthropic.com", clientPool),
		"gemini":         executor.NewGeminiExecutor("https://generativelanguage.googleapis.com/v1beta", clientPool),
		"github-copilot": executor.NewGitHubCopilotExecutor("https://api.githubcopilot.com", clientPool),
		"openrouter":     executor.NewDefaultExecutor("openrouter", "https://openrouter.ai/api/v1/chat/completions", clientPool),
		"ollama":         executor.NewDefaultExecutor("ollama", "http://localhost:11434/v1/chat/completions", clientPool),
		"default":        executor.NewDefaultExecutor("default", "", clientPool),
	}

	// Wrap executors with retry logic
	retryCfg := executor.DefaultRetryConfig()
	for id, exec := range executors {
		executors[id] = executor.NewRetryExecutor(exec, retryCfg)
	}

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

	// Create and start server
	srv := server.New(server.Config{
		Host:          *host,
		Port:          *port,
		DBPath:        *dbPath,
		DashboardFS:   dashboardFS,
		SetupToken:    setupToken,
	}, server.Dependencies{
		Store:             db,
		TranslateRegistry: translateReg,
		ProviderSelector:  providerSel,
		ProviderRegistry:  providerReg,
		Executors:         executors,
		UsageTracker:      usageTracker,
		Auth:              authMgr,
		OpenAIAuth:        oauth.NewOpenAIAuth(),
		SmartRouter:       routing.NewSmartRouter(),
		ConversationStore: routing.NewConversationStore(),
		BypassFilter:      bypass.NewFilter(),
		HealthChecker:     provider.NewHealthChecker(providerSel, 60*time.Second),
		RateLimiter:       ratelimit.New(),
	})

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
