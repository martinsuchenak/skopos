package main

import (
	"context"
	"os"

	"github.com/paularlott/cli"
	"github.com/paularlott/cli/env"
	cli_toml "github.com/paularlott/cli/toml"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/cmd"
)

var configFile = "skopos-config.toml"

func main() {
	log := logslog.New(logslog.Config{
		Level:  "info",
		Format: "console",
		Writer: os.Stdout,
	})

	_ = env.Load()

	app := &cli.Command{
		Name:       "skopos",
		Usage:      "skopos service",
		Version:    build.Version + " (" + build.Date + ")",
		ConfigFile: cli_toml.NewConfigFile(&configFile, nil),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:         "config",
				DefaultValue: "skopos-config.toml",
				Usage:        "Path to configuration file",
				EnvVars:      []string{"CONFIG_FILE"},
				AssignTo:     &configFile,
				Global:       true,
			},
			&cli.StringFlag{
				Name:         "log-level",
				DefaultValue: "info",
				Usage:        "Log level (debug, info, warn, error)",
				EnvVars:      []string{"LOG_LEVEL"},
				ConfigPath:   []string{"log.level"},
				Global:       true,
			},
			&cli.StringFlag{
				Name:         "log-format",
				DefaultValue: "text",
				Usage:        "Log format (text, json)",
				EnvVars:      []string{"LOG_FORMAT"},
				ConfigPath:   []string{"log.format"},
				Global:       true,
			},
		},
		Commands: cmd.Commands(),
	}

	if err := app.Execute(context.Background()); err != nil {
		log.Error("application error", "error", err)
		os.Exit(1)
	}
}
