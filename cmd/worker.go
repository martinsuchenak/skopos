package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/BurntSushi/toml"
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
			&cli.StringFlag{Name: "state-dir", Usage: "Worktrees, logs and session state", EnvVars: []string{"SKOPOS_WORKER_STATE"}, DefaultValue: defaultStateDir},
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
			// Flags carry the minimum; the TOML [worker] section (repos,
			// accounts, timeouts, MCP deny) rides on top when present.
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
// accounts, timeouts, MCP deny) from the skopos config file. Missing
// file/section is fine — the flags carry the minimum. Without this the
// daemon claims runs it cannot execute (review finding 3).
func loadWorkerConfigFile(cfg *worker.Config, path string) error {
	if path == "" {
		path = "skopos-config.toml"
	}
	if _, err := os.Stat(path); err != nil {
		return nil // absent config: flags only
	}
	var file struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.DecodeFile(path, &file); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if file.Worker == nil {
		return nil
	}
	w := file.Worker
	if cfg.ID == "" {
		cfg.ID = w.ID
	}
	if cfg.StateDir == "" || cfg.StateDir == defaultStateDir {
		cfg.StateDir = w.StateDir
	}
	if len(w.Runners) > 0 {
		cfg.Runners = w.Runners
	}
	if len(w.Workspaces) > 0 {
		cfg.Workspaces = w.Workspaces
	}
	if w.PollEvery > 0 {
		cfg.PollEvery = w.PollEvery
	}
	cfg.Repos = w.Repos
	cfg.Accounts = w.Accounts
	if w.Bin != "" {
		cfg.Bin = w.Bin
	}
	if w.PlanTimeout > 0 {
		cfg.PlanTimeout = w.PlanTimeout
	}
	if w.ImplTimeout > 0 {
		cfg.ImplTimeout = w.ImplTimeout
	}
	if len(w.MCPDeny) > 0 {
		cfg.MCPDeny = w.MCPDeny
	}
	if len(w.MCPServers) > 0 {
		cfg.MCPServers = w.MCPServers
	}
	return nil
}

const defaultStateDir = "~/.local/state/skopos-worker"
