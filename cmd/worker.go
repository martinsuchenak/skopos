package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
// Invoked bare it runs the daemon; `worker init/install/uninstall` manage
// the config and the launchd/systemd service.
func workerCmd() *cli.Command {
	return &cli.Command{
		Name:  "worker",
		Usage: "Run the execution worker: claim run stages from skopos and execute them (long-running daemon)",
		Flags: []cli.Flag{
			// No ConfigPath on these two (review N1): the CLI library cannot
			// tell a config value from an explicit flag, which made the
			// config silently beat the env file. The command resolves
			// flag -> environment (incl. the env file) -> [client] itself.
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL (also read from [client] as the last fallback)", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key, worker scope (also read from [client] as the last fallback)", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "worker-id", Usage: "Worker identity (defaults to worker-<hostname>)", EnvVars: []string{"SKOPOS_WORKER_ID"}},
			&cli.StringFlag{Name: "state-dir", Usage: "Worktrees, logs and session state", EnvVars: []string{"SKOPOS_WORKER_STATE"}, DefaultValue: defaultStateDir},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace ID this worker serves (repeatable; must have a local clone configured)"},
			&cli.StringFlag{Name: "env-file", Usage: "KEY=VALUE file loaded before the config (secrets for service installs; real env wins)", EnvVars: []string{"SKOPOS_WORKER_ENV"}},
		},
		Commands: []*cli.Command{
			workerInitCmd(),
			workerInstallCmd(),
			workerCheckCmd(),
			workerUninstallCmd(),
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			// The env file loads FIRST (review H1): flag env vars were
			// resolved at parse time, so anything it provides must be
			// picked up explicitly below, flag → environment → config.
			fileKeys, warnings, err := loadWorkerEnvFile(workerEnvFileFlag(cmd))
			if err != nil {
				return err
			}
			clientCfg, err := readClientSection(cmd.GetString("config"))
			if err != nil {
				return err
			}
			key := resolveClientSetting(cmd.GetString("api-key"), "SKOPOS_API_KEY", "", clientCfg.APIKey)
			if key == "" {
				return fmt.Errorf("--api-key or SKOPOS_API_KEY is required (mint a worker-scoped key: skopos key create --name <host> --workspace <id>)")
			}
			home, _ := os.UserHomeDir()
			cfg := &worker.Config{
				ID:         resolveWorkerSetting(cmd.GetString("worker-id"), "SKOPOS_WORKER_ID", ""),
				StateDir:   resolveWorkerSetting(cmd.GetString("state-dir"), "SKOPOS_WORKER_STATE", defaultStateDir),
				Workspaces: cmd.GetStringSlice("workspace"),
			}
			// Flags carry the minimum; the TOML [worker] section (repos,
			// accounts, timeouts, MCP deny) rides on top when present.
			// StateDir stays unexpanded here so the config comparison sees
			// the flag default — Default() expands ~ for everything (H2).
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
			for _, warn := range warnings {
				log.Warn(warn)
			}
			serverURL := resolveClientSetting(cmd.GetString("server-url"), "SKOPOS_SERVER_URL", "http://localhost:8080", clientCfg.ServerURL)
			client := worker.NewClient(expandTilde(serverURL, home), key)
			w := worker.New(cfg, client, log)
			// Env-file values never reach agent runs (review low): the
			// scrubber drops every key the file provided.
			w.SetEnvFileKeys(fileKeys)

			runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log.Info("worker starting", "workspaces", cfg.Workspaces, "runners", cfg.Runners, "state", cfg.StateDir)
			// A cancelled context is a clean stop (SIGTERM from the service
			// manager) — exit 0 so Restart=on-failure stays quiet.
			if err := w.Loop(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
}

// resolveWorkerSetting picks the flag value when one was given explicitly,
// else the environment (which now includes the env file), else the flag's
// default. Flag values equal to the default cannot be distinguished from
// unset — an explicit flag matching the default loses to the environment,
// which is the safe direction (nobody types the default on purpose).
func resolveWorkerSetting(flagVal, envName, flagDefault string) string {
	if flagVal != "" && flagVal != flagDefault {
		return flagVal
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	if flagVal != "" {
		return flagVal
	}
	return flagDefault
}

// workerEnvFileFlag resolves the env file: an explicit --env-file, else the
// skopos-worker.env next to the config when it exists (the service installs
// never embed secrets in the unit/plist — the daemon pulls them in itself).
func workerEnvFileFlag(cmd *cli.Command) string {
	return defaultWorkerEnvFile(configFileFor(cmd), cmd.GetString("env-file"))
}

// defaultWorkerEnvFile is the shared lookup for the daemon, check and
// install (review N1 low: a custom --env-file must not make them disagree).
func defaultWorkerEnvFile(configPath, explicit string) string {
	if explicit != "" {
		return explicit
	}
	p := filepath.Join(filepath.Dir(configPath), "skopos-worker.env")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// readClientSection reads [client] from the config file (the last fallback
// for the server URL and API key).
func readClientSection(configPath string) (struct{ ServerURL, APIKey string }, error) {
	var out struct{ ServerURL, APIKey string }
	var file struct {
		Client struct {
			ServerURL string `toml:"server_url"`
			APIKey    string `toml:"api_key"`
		}
	}
	if _, err := toml.DecodeFile(configPath, &file); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("parsing %s: %w", configPath, err)
	}
	out.ServerURL, out.APIKey = file.Client.ServerURL, file.Client.APIKey
	return out, nil
}

// resolveClientSetting is the ONE precedence chain for settings that can
// also come from [client]: explicit flag, then environment (which includes
// the env file by the time this runs), then the config, then the default.
func resolveClientSetting(flagVal, envName, flagDefault, cfgVal string) string {
	if flagVal != "" && flagVal != flagDefault {
		return flagVal
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	if cfgVal != "" {
		return cfgVal
	}
	if flagVal != "" {
		return flagVal
	}
	return flagDefault
}

// loadWorkerEnvFile applies KEY=VALUE lines via os.Setenv for keys not
// already set — the real environment always wins. It accepts the usual
// dotenv forms (quoted values, `export` prefixes, spaces around `=`) and
// returns the keys it set (the scrubber drops them from agent runs) plus
// warnings (a group/world-readable file holds secrets).
func loadWorkerEnvFile(path string) (keys []string, warnings []string, err error) {
	if path == "" {
		return nil, nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading env file: %w", err)
	}
	if st, serr := os.Stat(path); serr == nil && st.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("%s is group/world readable (chmod 600 %s) — it holds secrets", path, path))
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, nil, fmt.Errorf("%s: not KEY=VALUE: %q", path, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
			keys = append(keys, k)
		}
	}
	return keys, warnings, nil
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
