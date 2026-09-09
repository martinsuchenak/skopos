package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/cmd/mcp"
	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/cleanup"
	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/health"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	// go-scaffolder:serve-imports
)

func init() {
	Register(serveCmd())
}

func serveCmd() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Start the skopos service",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:         "server-host",
				DefaultValue: "127.0.0.1",
				Usage:        "Server listen host (set 0.0.0.0 to listen on all interfaces)",
				ConfigPath:   []string{"server.host"},
				EnvVars:      []string{"SERVER_HOST"},
			},
			&cli.IntFlag{
				Name:         "server-port",
				DefaultValue: 8080,
				Usage:        "Server listen port (HTTP, REST, dashboard, and MCP at /mcp)",
				ConfigPath:   []string{"server.port"},
				EnvVars:      []string{"SERVER_PORT"},
			},
			&cli.StringFlag{
				Name:         "database-path",
				DefaultValue: "skopos.db",
				Usage:        "SQLite database path",
				ConfigPath:   []string{"database.path"},
				EnvVars:      []string{"DATABASE_PATH"},
			},
			&cli.StringFlag{
				Name:       "api-key",
				Usage:      "API key required for all API endpoints (reads, writes, MCP, SSE); empty disables auth (loopback binds only, unless --insecure-no-api-key)",
				ConfigPath: []string{"auth.api_key"},
				EnvVars:    []string{"SKOPOS_API_KEY"},
			},
			&cli.BoolFlag{
				Name:    "insecure-no-api-key",
				Usage:   "Allow starting without an api_key on a non-loopback interface (every endpoint is open to the network)",
				EnvVars: []string{"SKOPOS_INSECURE_NO_API_KEY"},
			},
			&cli.IntFlag{
				Name:         "health-stuck-threshold",
				DefaultValue: 15,
				Usage:        "Minutes before an active agent is marked stuck (0 to disable)",
				ConfigPath:   []string{"health.stuck_threshold_minutes"},
				EnvVars:      []string{"HEALTH_STUCK_THRESHOLD"},
			},
			&cli.StringFlag{
				Name:         "index-dir",
				DefaultValue: "indexes",
				Usage:        "Directory for per-workspace code index databases",
				ConfigPath:   []string{"codeindex.dir"},
				EnvVars:      []string{"SKOPOS_INDEX_DIR"},
			},
			&cli.StringFlag{
				Name:       "embeddings-url",
				Usage:      "OpenAI-compatible /v1 embeddings endpoint for semantic code search (empty disables; local Ollama e.g. http://localhost:11434/v1)",
				ConfigPath: []string{"codeindex.embeddings.url"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_URL"},
			},
			&cli.StringFlag{
				Name:       "embeddings-model",
				Usage:      "Embedding model name (required with --embeddings-url)",
				ConfigPath: []string{"codeindex.embeddings.model"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_MODEL"},
			},
			&cli.StringFlag{
				Name:       "embeddings-api-key",
				Usage:      "API key for the embeddings endpoint (not needed for local servers)",
				ConfigPath: []string{"codeindex.embeddings.api_key"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_API_KEY"},
			},
			&cli.StringFlag{
				Name:         "vector-store",
				DefaultValue: "sqlite",
				Usage:        "Vector backend for embeddings: sqlite (embedded, brute force) or qdrant (external, monorepo scale)",
				ConfigPath:   []string{"codeindex.embeddings.vector_store"},
				EnvVars:      []string{"SKOPOS_VECTOR_STORE"},
			},
			&cli.StringFlag{
				Name:       "qdrant-url",
				Usage:      "Qdrant REST address (e.g. http://localhost:6333 or https://qdrant.example.com) when --vector-store=qdrant",
				ConfigPath: []string{"codeindex.embeddings.qdrant_url"},
				EnvVars:    []string{"SKOPOS_QDRANT_URL"},
			},
			&cli.StringFlag{
				Name:       "qdrant-api-key",
				Usage:      "Qdrant API key (when required)",
				ConfigPath: []string{"codeindex.embeddings.qdrant_api_key"},
				EnvVars:    []string{"SKOPOS_QDRANT_API_KEY"},
			},
			&cli.IntFlag{
				Name:         "cleanup-retention-days",
				DefaultValue: 30,
				Usage:        "Days to retain data before automatic cleanup (0 to disable)",
				ConfigPath:   []string{"cleanup.retention_days"},
				EnvVars:      []string{"CLEANUP_RETENTION_DAYS"},
			},
		},
		// go-scaffolder:serve-flags
		Run: func(ctx context.Context, cmd *cli.Command) error {
			log := logslog.New(logslog.Config{
				Level:  cmd.GetString("log-level"),
				Format: cmd.GetString("log-format"),
				Writer: os.Stdout,
			})
			log.Info("starting skopos service")

			apiKey := cmd.GetString("api-key")
			if apiKey == "" {
				if !isLoopbackHost(cmd.GetString("server-host")) && !cmd.GetBool("insecure-no-api-key") {
					return fmt.Errorf("refusing to start without an api_key on a non-loopback interface: set --api-key / SKOPOS_API_KEY, or pass --insecure-no-api-key to explicitly accept that every endpoint is open to the network")
				}
				log.Warn("no api_key configured: authentication is disabled (all endpoints are open)")
			}

			rest.SetLogger(log)

			sqlDB, err := db.Connect(log, cmd.GetString("database-path"))
			if err != nil {
				return err
			}
			defer sqlDB.Close()
			if err := db.RunMigrations(sqlDB); err != nil {
				return err
			}

			statusService := status.NewService(status.NewStorage(sqlDB))
			statusHandler := status.NewHandler(statusService, apiKey)

			blackboardService := blackboard.NewService(blackboard.NewStorage(sqlDB))
			blackboardHandler := blackboard.NewHandler(blackboardService, apiKey)

			plansStorage := plans.NewStorage(sqlDB)
			plansService := plans.NewService(plansStorage)
			plansHandler := plans.NewHandler(plansService, apiKey)

			workspacesService := workspaces.NewService(workspaces.NewStorage(sqlDB))
			workspacesHandler := workspaces.NewHandler(workspacesService, apiKey)

			// Code index: one SQLite DB per workspace under --index-dir.
			codeIndexStore, err := codeindex.NewStore(cmd.GetString("index-dir"))
			if err != nil {
				return err
			}
			defer codeIndexStore.Close()
			codeIndexService := codeindex.NewService(codeIndexStore)
			codeIndexHandler := codeindex.NewHandler(codeIndexService, apiKey)
			codeIndexHandler.SetWorkspaceRegistrar(func(id string) {
				// First push registers the workspace so it persists in the registry.
				_, _, _ = workspacesService.Create(context.Background(), workspaces.CreateInput{ID: id})
			})
			// Server-side indexing: clone/pull the registered git_url and rebuild.
			refresher, err := codeindex.NewRefresher(codeIndexStore, cmd.GetString("index-dir"), func(id string) (string, error) {
				ws, err := workspacesService.Get(context.Background(), id)
				if err != nil {
					return "", err
				}
				return ws.GitURL, nil
			})
			if err != nil {
				return err
			}
			codeIndexHandler.SetRefresher(refresher)

			// Optional semantic embeddings: any OpenAI-compatible endpoint
			// (local Ollama keeps everything on-host). Disabled by default.
			// Optional external vector backend (default: embedded SQLite).
			if vs := cmd.GetString("vector-store"); vs == "qdrant" {
				if cmd.GetString("qdrant-url") == "" {
					return fmt.Errorf("--qdrant-url is required when --vector-store=qdrant")
				}
				qd, err := codeindex.NewQdrantVectorStore(cmd.GetString("qdrant-url"), cmd.GetString("qdrant-api-key"))
				if err != nil {
					return err
				}
				codeIndexService.SetVectorStore(qd)
				log.Info("vector store: qdrant", "url", cmd.GetString("qdrant-url"))
			}
			if embURL := cmd.GetString("embeddings-url"); embURL != "" && cmd.GetString("embeddings-model") != "" {
				embedder := &codeindex.OpenAIEmbedder{
					BaseURL:   embURL,
					ModelName: cmd.GetString("embeddings-model"),
					APIKey:    cmd.GetString("embeddings-api-key"),
				}
				codeIndexHandler.SetEmbeddingManager(codeindex.NewEmbeddingManager(codeIndexService, embedder))
				log.Info("semantic code search enabled", "model", embedder.ModelName, "url", embURL, "vectors", codeIndexService.VectorStoreName())
			}

			// Cancel background work and initiate graceful shutdown on SIGINT/SIGTERM.
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// Created before the background workers so they can publish events
			// for their out-of-band mutations.
			hub := events.NewHub()

			stuckThreshold := cmd.GetInt("health-stuck-threshold")
			if stuckThreshold > 0 {
				health.NewChecker(sqlDB, time.Duration(stuckThreshold)*time.Minute, log, hub).Start(ctx)
			}
			retentionDays := cmd.GetInt("cleanup-retention-days")
			if retentionDays > 0 {
				cleanupRetention := time.Duration(retentionDays) * 24 * time.Hour
				cleanup.NewCleaner(sqlDB, cleanupRetention, log, hub).Start(ctx)
			}
			// go-scaffolder:serve-init

			mux := http.NewServeMux()
			routes.RegisterRoutes(mux, statusHandler, blackboardHandler, plansHandler, workspacesHandler, codeIndexHandler)
			mux.Handle("GET /api/events/stream", auth.APIKeyMiddleware(apiKey)(events.StreamHandler(hub)))

			// Runtime metrics are not part of the product API: require the API key
			// when auth is enabled (the middleware is a no-op otherwise).
			mux.Handle("GET /metrics", auth.APIKeyMiddleware(apiKey)(http.HandlerFunc(routes.MetricsHandler)))

			// MCP endpoint, mounted on the same server/port as everything else. Body
			// is capped like the REST API (rest.DecodeJSON applies its cap only to
			// handlers that decode via it).
			mcpHandler := mcp.NewMCPHandler(statusService, blackboardService, plansService, codeIndexService)
			mcpHandler = rest.BodyLimit(noBrowserOrigin(mcpHandler))
			if apiKey != "" {
				mcpHandler = auth.APIKeyMiddleware(apiKey)(mcpHandler)
			}
			for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
				mux.Handle(m+" /mcp", mcpHandler)
			}

			httpServer := &http.Server{
				Addr:              fmt.Sprintf("%s:%d", cmd.GetString("server-host"), cmd.GetInt("server-port")),
				Handler:           hostAllowed(cmd.GetString("server-host"))(events.Middleware(hub, log, mux)),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second, // SSE handler clears this per-request
				IdleTimeout:       120 * time.Second,
			}
			// Closing the hub ends open SSE streams, so Shutdown() can complete
			// promptly instead of waiting out its timeout on live connections.
			httpServer.RegisterOnShutdown(hub.Close)

			httpErr := make(chan error, 1)
			go func() {
				log.Info("starting HTTP server", "addr", httpServer.Addr, "mcp", "/mcp")
				if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					httpErr <- err
				}
			}()

			select {
			case <-ctx.Done():
				log.Info("shutdown signal received")
			case err := <-httpErr:
				stop()
				return fmt.Errorf("http server: %w", err)
			}

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				log.Error("http shutdown error", "error", err)
			}
			return nil
		},
	}
}

// noBrowserOrigin rejects cross-origin MCP requests: real MCP clients never
// send an Origin header, while browsers always do. Without this, the MCP
// handler's unconditional "Access-Control-Allow-Origin: *" turns any website
// in the victim's browser into an MCP client against unauthenticated local
// instances. Preflight OPTIONS requests are denied as well, so the browser
// blocks the call before it reaches the MCP handler.
func noBrowserOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross-origin MCP requests are not permitted", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether host is "localhost" or a loopback IP, i.e.
// the server is not reachable from other machines.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostAllowed returns middleware that validates the Host header of every
// request when the server is bound to a loopback address. Browsers treat a
// DNS name that resolves to 127.0.0.1 as same-origin with a loopback service
// (DNS rebinding), so without this check any visited website could read all
// API responses through such a name. Non-loopback binds are not restricted:
// clients legitimately reach those by arbitrary hostnames or IPs.
func hostAllowed(serverHost string) func(http.Handler) http.Handler {
	enforce := isLoopbackHost(serverHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if enforce {
				host := strings.ToLower(r.Host)
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
				if !isLoopbackHost(host) {
					http.Error(w, "host not allowed", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
