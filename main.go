package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/paularlott/cli"
	"github.com/paularlott/cli/env"
	cli_toml "github.com/paularlott/cli/toml"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/cmd"
)

var configFile = "skopos-config.toml"

func main() {
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
		printError(err)
		os.Exit(1)
	}
}

// printError renders a command failure for terminal use: the bare message
// prefixed with the binary name, no timestamps or level tags — the slog
// format is for the server's streamed logs, not for a human at a prompt.
func printError(err error) {
	printErrorTo(os.Stderr, err)
}

func printErrorTo(w io.Writer, err error) {
	_, _ = fmt.Fprintln(w, "skopos:", err)
}
