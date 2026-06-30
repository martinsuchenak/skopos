package cmd

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/martinsuchenak/skopos/internal/health"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"

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
				DefaultValue: "0.0.0.0",
				Usage:        "Server listen host",
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
				Usage:        "Minutes before an active agent is marked stuck",
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
			}

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

			// Cancel background work and initiate graceful shutdown on SIGINT/SIGTERM.
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			threshold := time.Duration(cmd.GetInt("health-stuck-threshold")) * time.Minute
			health.NewChecker(sqlDB, threshold, log).Start(ctx)
			retentionDays := cmd.GetInt("cleanup-retention-days")
			if retentionDays > 0 {
				cleanupRetention := time.Duration(retentionDays) * 24 * time.Hour
				cleanup.NewCleaner(sqlDB, cleanupRetention, log).Start(ctx)
			}
			// go-scaffolder:serve-init

			mux := http.NewServeMux()
			routes.RegisterRoutes(mux, statusHandler, blackboardHandler, plansHandler)

			// MCP endpoint, mounted on the same server/port as everything else.
			mcpHandler := mcp.NewMCPHandler(statusService, blackboardService, plansService)
			if apiKey != "" {
				mcpHandler = auth.APIKeyMiddleware(apiKey)(mcpHandler)
			}
			for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
				mux.Handle(m+" /mcp", mcpHandler)
			}

			httpServer := &http.Server{
				Addr:              fmt.Sprintf("%s:%d", cmd.GetString("server-host"), cmd.GetInt("server-port")),
				Handler:           mux,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			}

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
