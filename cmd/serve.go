package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/cleanup"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/health"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"

	mcpserver "github.com/martinsuchenak/skopos/cmd/mcp"
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
				Usage:        "Server listen port",
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
				Usage:      "API key required for write endpoints",
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

			conn, err := db.Connect(log, "localhost:0", "", "", cmd.GetString("database-path"))
			if err != nil {
				return err
			}
			defer conn.SQL.Close()
			if err := db.RunMigrations(conn.SQL); err != nil {
				return err
			}

			statusService := status.NewService(status.NewStorage(conn.SQL))
			statusHandler := status.NewHandler(statusService, cmd.GetString("api-key"))

			blackboardService := blackboard.NewService(blackboard.NewStorage(conn.SQL))
			blackboardHandler := blackboard.NewHandler(blackboardService, cmd.GetString("api-key"))

			plansStorage := plans.NewStorage(conn.SQL)
			plansService := plans.NewService(plansStorage)
			plansHandler := plans.NewHandler(plansService, cmd.GetString("api-key"))

			mcpserver.StartMCPServer(log, statusService, blackboardService, plansService)

			threshold := time.Duration(cmd.GetInt("health-stuck-threshold")) * time.Minute
			health.NewChecker(conn.SQL, threshold, log).Start(ctx)
			retentionDays := cmd.GetInt("cleanup-retention-days")
			if retentionDays > 0 {
				cleanupRetention := time.Duration(retentionDays) * 24 * time.Hour
				cleanup.NewCleaner(conn.SQL, cleanupRetention, log).Start(ctx)
			}
			// go-scaffolder:serve-init

			mux := http.NewServeMux()
			routes.RegisterRoutes(mux, statusHandler, blackboardHandler, plansHandler)

			addr := fmt.Sprintf("%s:%d", cmd.GetString("server-host"), cmd.GetInt("server-port"))
			log.Info("starting HTTP server", "addr", addr)
			// go-scaffolder:serve-start
			return http.ListenAndServe(addr, mux)
		},
	}
}
