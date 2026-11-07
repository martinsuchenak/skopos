package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/paularlott/cli"
	"github.com/paularlott/cli/env"
	cli_toml "github.com/paularlott/cli/toml"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/cmd"
	"github.com/martinsuchenak/skopos/internal/srvurl"
)

var configFile = "skopos-config.toml"

// configSearchPaths resolves the client config CWD-first, then globally:
// a repo-local skopos-config.toml wins, else ~/.config/skopos (explicitly,
// not os.UserConfigDir — on macOS that is ~/Library/Application Support,
// which nobody looks in for CLI configs; the XDG path is the convention
// users expect on every platform). Without the global fallback, every new
// checkout silently reverts to local mode even when a server is configured
// globally via `skopos install` — the CLI and hooks then disagree with the
// MCP config and steer agents toward starting a local server.
func configSearchPaths() []string {
	if home, err := os.UserHomeDir(); err == nil {
		return []string{".", filepath.Join(home, ".config", "skopos")}
	}
	return []string{"."}
}

func main() {
	_ = env.Load()
	// Every URL may name an SRV record: srv+http(s)://name/path.
	srvurl.Install()

	app := &cli.Command{
		Name:       "skopos",
		Usage:      "skopos service",
		Version:    build.String(),
		ConfigFile: cli_toml.NewConfigFile(&configFile, configSearchPaths),
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
			&cli.StringFlag{
				Name:       "dns-server",
				Usage:      "DNS server for srv+http(s):// URLs, host:port (e.g. Consul's 127.0.0.1:8600); default: the system resolver",
				EnvVars:    []string{"SKOPOS_DNS_SERVER"},
				ConfigPath: []string{"dns.server"},
				Global:     true,
			},
			&cli.StringFlag{
				Name:       "log-output-url",
				Usage:      "Also send logs to this HTTP log intake (e.g. http://victorialogs:9428/insert/jsonline, a Loki push URL, an Elasticsearch _bulk URL)",
				EnvVars:    []string{"LOG_OUTPUT_URL"},
				ConfigPath: []string{"log.output.url"},
				Global:     true,
			},
			&cli.StringFlag{
				Name:         "log-output-format",
				DefaultValue: "ndjson",
				Usage:        "Log output format (ndjson, loki, elasticsearch)",
				EnvVars:      []string{"LOG_OUTPUT_FORMAT"},
				ConfigPath:   []string{"log.output.format"},
				Global:       true,
			},
			&cli.StringFlag{
				Name:         "log-output-stream",
				DefaultValue: "skopos",
				Usage:        "Log output stream name (VictoriaLogs stream, Loki job, Elasticsearch index)",
				EnvVars:      []string{"LOG_OUTPUT_STREAM"},
				ConfigPath:   []string{"log.output.stream"},
				Global:       true,
			},
			&cli.StringFlag{
				Name:       "log-output-header",
				Usage:      "Header(s) sent to the log output, \"Name: value\" (separate several with ;)",
				EnvVars:    []string{"LOG_OUTPUT_HEADER"},
				ConfigPath: []string{"log.output.header"},
				Global:     true,
			},
		},
		Commands: cmd.Commands(),
		PreRun: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			return ctx, srvurl.SetDNSServer(c.GetString("dns-server"))
		},
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
