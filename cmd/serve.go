package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/cmd/mcp"
	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/cleanup"
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
				Usage:      "API key required for write endpoints and MCP",
				ConfigPath: []string{"auth.api_key"},
				EnvVars:    []string{"SKOPOS_API_KEY"},
			},
			&cli.IntFlag{
				Name:         "health-stuck-threshold",
				DefaultValue: 15,
				Usage:        "Minutes before an active agent is marked stuck (0 to disable)",
				ConfigPath:   []string{"health.stuck_threshold_minutes"},
				EnvVars:      []string{"HEALTH_STUCK_THRESHOLD"},
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
				log.Warn("no api_key configured: authentication is disabled (all endpoints are open)")
				if !isLoopbackHost(cmd.GetString("server-host")) {
					log.Warn("binding a non-loopback interface without an api_key exposes all endpoints to the network")
				}
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
			routes.RegisterRoutes(mux, statusHandler, blackboardHandler, plansHandler, workspacesHandler)
			mux.HandleFunc("GET /api/events/stream", events.StreamHandler(hub))

			// Runtime metrics are not part of the product API: require the API key
			// when auth is enabled (the middleware is a no-op otherwise).
			mux.Handle("GET /metrics", auth.APIKeyMiddleware(apiKey)(http.HandlerFunc(routes.MetricsHandler)))

			// MCP endpoint, mounted on the same server/port as everything else. Body
			// is capped like the REST API (rest.DecodeJSON applies its cap only to
			// handlers that decode via it).
			mcpHandler := mcp.NewMCPHandler(statusService, blackboardService, plansService)
			mcpHandler = rest.BodyLimit(mcpHandler)
			if apiKey != "" {
				mcpHandler = auth.APIKeyMiddleware(apiKey)(mcpHandler)
			}
			for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
				mux.Handle(m+" /mcp", mcpHandler)
			}

			httpServer := &http.Server{
				Addr:              fmt.Sprintf("%s:%d", cmd.GetString("server-host"), cmd.GetInt("server-port")),
				Handler:           events.Middleware(hub, log, mux),
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

// isLoopbackHost reports whether host is "localhost" or a loopback IP, i.e.
// the server is not reachable from other machines.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
