package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/bypass"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/ratelimit"
	"sage-router/internal/routing"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	"sage-router/internal/usage"
)

// Config holds server configuration.
type Config struct {
	Host   string
	Port   int
	DBPath string

	// Dashboard static files (embedded or from filesystem)
	DashboardFS fs.FS

	// One-time setup token for first run (empty if password already set)
	SetupToken string
}

// Dependencies holds all injected dependencies.
type Dependencies struct {
	Store              store.Store
	TranslateRegistry  *translate.Registry
	ProviderSelector   *provider.Selector
	ProviderRegistry   *provider.Registry
	Executors          map[string]executor.Executor
	UsageTracker       *usage.Tracker
	Auth               *auth.Manager
	OpenAIAuth         *oauth.OpenAIAuth
	SmartRouter        *routing.SmartRouter
	ConversationStore  *routing.ConversationStore
	BypassFilter       *bypass.Filter
	HealthChecker      *provider.HealthChecker
	RateLimiter        *ratelimit.Limiter
}

// Server is the main HTTP server for sage-router.
type Server struct {
	config  Config
	deps    Dependencies
	httpSrv *http.Server
	mux     *http.ServeMux
}

// New creates a new Server.
func New(cfg Config, deps Dependencies) *Server {
	s := &Server{
		config: cfg,
		deps:   deps,
		mux:    http.NewServeMux(),
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	// v1 API routes (OpenAI-compatible)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("POST /v1/messages", s.handleChatCompletions) // Claude format
	s.mux.HandleFunc("GET /v1/models", s.handleListModels)
	s.mux.HandleFunc("POST /v1/responses", s.handleChatCompletions) // Responses API

	// Dashboard management API — public auth routes
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/auth/check", s.handleAuthCheck)
	s.mux.HandleFunc("GET /api/auth/token-login", s.handleTokenLogin)
	s.mux.HandleFunc("POST /api/auth/setup", s.handleSetup)

	// Dashboard management API — protected routes
	guard := &AuthGuardMiddleware{
		ValidateToken: func(token string) (bool, error) {
			return s.deps.Auth.ValidateToken(token)
		},
		PublicPaths: []string{"/api/auth/"},
	}
	protect := func(h http.HandlerFunc) http.Handler {
		return guard.Middleware(h)
	}

	s.mux.Handle("GET /api/connections", protect(s.handleListConnections))
	s.mux.Handle("POST /api/connections", protect(s.handleCreateConnection))
	s.mux.Handle("PUT /api/connections/{id}", protect(s.handleUpdateConnection))
	s.mux.Handle("DELETE /api/connections/{id}", protect(s.handleDeleteConnection))
	s.mux.Handle("POST /api/connections/{id}/test", protect(s.handleTestConnection))

	s.mux.Handle("GET /api/combos", protect(s.handleListCombos))
	s.mux.Handle("POST /api/combos", protect(s.handleCreateCombo))
	s.mux.Handle("PUT /api/combos/{id}", protect(s.handleUpdateCombo))
	s.mux.Handle("DELETE /api/combos/{id}", protect(s.handleDeleteCombo))

	s.mux.Handle("GET /api/aliases", protect(s.handleListAliases))
	s.mux.Handle("POST /api/aliases", protect(s.handleSetAlias))
	s.mux.Handle("DELETE /api/aliases/{name}", protect(s.handleDeleteAlias))

	s.mux.Handle("GET /api/keys", protect(s.handleListAPIKeys))
	s.mux.Handle("POST /api/keys", protect(s.handleCreateAPIKey))
	s.mux.Handle("PATCH /api/keys/{id}", protect(s.handleUpdateAPIKey))
	s.mux.Handle("DELETE /api/keys/{id}", protect(s.handleDeleteAPIKey))

	s.mux.Handle("GET /api/settings", protect(s.handleGetSettings))
	s.mux.Handle("PUT /api/settings", protect(s.handleUpdateSettings))

	s.mux.Handle("GET /api/usage", protect(s.handleGetUsage))
	s.mux.Handle("GET /api/usage/summary", protect(s.handleGetUsageSummary))

	s.mux.Handle("GET /api/providers", protect(s.handleListProviders))
	s.mux.Handle("GET /api/models", protect(s.handleListModelCatalog))

	s.mux.Handle("GET /api/routing/summary", protect(s.handleGetRoutingSummary))
	s.mux.Handle("GET /api/routing/log", protect(s.handleGetRoutingLog))

	s.mux.Handle("GET /api/status", protect(s.handleStatus))
	s.mux.Handle("GET /api/detect/claude", protect(s.handleDetectClaude))

	// OpenAI OAuth device code flow
	s.mux.Handle("POST /api/oauth/openai/device", protect(s.handleOpenAIDeviceStart))
	s.mux.Handle("POST /api/oauth/openai/poll", protect(s.handleOpenAIDevicePoll))

	// Dashboard SPA — serve static files with fallback to index.html
	if s.config.DashboardFS != nil {
		s.mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", &spaHandler{fs: s.config.DashboardFS}))
		s.mux.Handle("GET /assets/", http.StripPrefix("/", http.FileServerFS(s.config.DashboardFS)))
	}

	// Root redirect
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/dashboard/", http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	})
}

// Handler returns the full middleware chain.
func (s *Server) Handler() http.Handler {
	var handler http.Handler = s.mux
	handler = CORSMiddleware(handler)
	handler = LoggingMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	return handler
}

// ListenAndServe starts the HTTP server with graceful shutdown.
func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)

	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // Disabled for SSE streaming
		IdleTimeout:  120 * time.Second,
	}

	// Start listener
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	// Print startup banner
	printBanner(s.config)

	// Start background health checker
	if s.deps.HealthChecker != nil {
		s.deps.HealthChecker.Start()
	}

	// Graceful shutdown
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpSrv.Serve(ln)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		if err != http.ErrServerClosed {
			return err
		}
	}

	return s.Shutdown()
}

// Shutdown performs graceful shutdown.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	slog.Info("draining connections", "timeout", "30s")

	if err := s.httpSrv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	// Stop health checker
	if s.deps.HealthChecker != nil {
		s.deps.HealthChecker.Stop()
	}

	// Flush usage tracker
	if s.deps.UsageTracker != nil {
		s.deps.UsageTracker.Close()
	}

	// Close store
	if s.deps.Store != nil {
		s.deps.Store.Close()
	}

	slog.Info("server stopped")
	return nil
}

func printBanner(cfg Config) {
	host := cfg.Host
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	baseURL := fmt.Sprintf("http://%s:%d", host, cfg.Port)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Sage Router")
	fmt.Fprintln(os.Stderr)

	if cfg.SetupToken != "" {
		// First run — print clickable setup URL
		setupURL := fmt.Sprintf("%s/dashboard/?token=%s", baseURL, cfg.SetupToken)
		fmt.Fprintf(os.Stderr, "  %s\n", setupURL)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  Open this link to get started.")
		fmt.Fprintln(os.Stderr, "  The link expires in 5 minutes.")
	} else {
		fmt.Fprintf(os.Stderr, "  Dashboard:  %s/dashboard/\n", baseURL)
		fmt.Fprintf(os.Stderr, "  API:        %s/v1\n", baseURL)
	}

	fmt.Fprintln(os.Stderr)
}

// spaHandler serves a SPA — returns index.html for non-file routes.
type spaHandler struct {
	fs fs.FS
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "" || path == "/" {
		path = "index.html"
	}

	// Try to serve the file directly
	f, err := h.fs.Open(path)
	if err == nil {
		f.Close()
		http.FileServerFS(h.fs).ServeHTTP(w, r)
		return
	}

	// Fallback to index.html for SPA routing
	r.URL.Path = "/"
	http.FileServerFS(h.fs).ServeHTTP(w, r)
}

// EmbedDashboard is a placeholder for the embedded dashboard files.
// The actual embed directive is in web/embed.go.
var EmbedDashboard embed.FS
