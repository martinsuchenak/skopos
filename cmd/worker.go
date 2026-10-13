package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/paularlott/cli"
)

func init() {
	Register(workerCmd())
}

// workerCmd is `skopos worker`: the execution plane as a subcommand of the
// same binary (the nomad/consul one-binary pattern). Deployment-agnostic —
// systemd, a container entrypoint, or Nomad all work; it needs only skopos
// over HTTP, a local clone per repo, and the agent CLIs on the host.
func workerCmd() *cli.Command {
	return &cli.Command{
		Name:  "worker",
		Usage: "Run the execution worker: claim run stages from skopos and execute them (long-running daemon)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key (worker scope)", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "worker-id", Usage: "Worker identity (defaults to worker-<hostname>)", EnvVars: []string{"SKOPOS_WORKER_ID"}},
			&cli.StringFlag{Name: "state-dir", Usage: "Worktrees, logs and session state", EnvVars: []string{"SKOPOS_WORKER_STATE"}, DefaultValue: "~/.local/state/skopos-worker"},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace ID this worker serves (repeatable; must have a local clone configured)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			key := cmd.GetString("api-key")
			if key == "" {
				return fmt.Errorf("--api-key or SKOPOS_API_KEY is required (mint a worker-scoped key: skopos key create --name <host> --workspace <id>)")
			}
			home, _ := os.UserHomeDir()
			cfg := &worker.Config{
				ID:         cmd.GetString("worker-id"),
				StateDir:   expandTilde(cmd.GetString("state-dir"), home),
				Workspaces: cmd.GetStringSlice("workspace"),
			}
			// Single-runner default: claude with the ambient login. The
			// TOML [worker] section (accounts, repos, timeouts) rides on
			// top when present.
			if err := loadWorkerConfigFile(cfg, cmd.GetString("config")); err != nil {
				return err
			}
			if err := cfg.Default(home); err != nil {
				return err
			}
			if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
				return err
			}

			log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("worker", cfg.ID)
			client := worker.NewClient(expandTilde(cmd.GetString("server-url"), home), key)
			w := worker.New(cfg, client, log)

			runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log.Info("worker starting", "workspaces", cfg.Workspaces, "runners", cfg.Runners, "state", cfg.StateDir)
			return w.Loop(runCtx)
		},
	}
}

func expandTilde(p, home string) string {
	if len(p) > 1 && p[0] == '~' {
		return filepath.Join(home, p[1:])
	}
	return p
}

// loadWorkerConfigFile reads the optional [worker] TOML section (repos,
// accounts, timeouts) from the skopos config file. Missing file/section is
// fine — the flags carry the minimum.
func loadWorkerConfigFile(cfg *worker.Config, path string) error {
	// The full TOML loader lives with the server config; for the worker we
	// accept the documented YAML-free subset via env/flags plus this hook.
	// Parsing a TOML [worker] table is added when the config shape settles.
	return nil
}
