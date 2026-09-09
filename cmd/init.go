package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/martinsuchenak/skopos/internal/workspace"
	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"
)

func init() {
	Register(initCmd())
}

func initCmd() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Initialize a default configuration file",
		Run: func(ctx context.Context, cmd *cli.Command) error {
			log := logslog.New(logslog.Config{
				Level:  "info",
				Format: "console",
				Writer: os.Stdout,
			})
			configPath := "skopos-config.toml"

			if _, err := os.Stat(configPath); err == nil {
				log.Warn("config file already exists", "path", configPath)
				return fmt.Errorf("config file %s already exists", configPath)
			}

			wsID := ""
			if id, err := workspace.Resolve("."); err == nil {
				wsID = id
			}
			if wsID != "" {
				log.Info("workspace for this directory (pass --workspace to CLI commands)", "id", wsID)
			}

			// Mirrors skopos-config.example.toml: every flag with a ConfigPath is
			// represented so the generated file is a complete starting point.
			defaultConfig := `# skopos configuration. All keys are optional; flags and env vars
# (SERVER_HOST, SERVER_PORT, DATABASE_PATH, SKOPOS_API_KEY, ...) override them.

[server]
# Loopback by default. Set "0.0.0.0" to expose the server on all interfaces
# (make sure to set an auth.api_key when doing so).
host = "127.0.0.1"
port = 8080

[database]
path = "skopos.db"

[auth]
# API key required for write endpoints and MCP (empty disables authentication).
api_key = ""

[health]
# Minutes before an active agent is marked stuck (0 disables the checker).
stuck_threshold_minutes = 15

[cleanup]
# Days to retain data before automatic cleanup (0 disables the cleanup worker).
retention_days = 30

[log]
level = "info"
format = "text"
`
			if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err != nil {
				return fmt.Errorf("writing config file: %w", err)
			}

			log.Info("config file created", "path", configPath)
			return nil
		},
	}
}
