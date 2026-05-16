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
			} else {
				log.Warn("could not resolve workspace", "error", err.Error())
			}

			defaultConfig := fmt.Sprintf(`[workspace]
id = "%s"

[log]
level = "info"
format = "text"
`, wsID)
			if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err != nil {
				return fmt.Errorf("writing config file: %w", err)
			}

			log.Info("config file created", "path", configPath)
			return nil
		},
	}
}
