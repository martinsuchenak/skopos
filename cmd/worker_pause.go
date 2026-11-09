package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/srvurl"
	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/paularlott/cli"
)

// skopos worker pause|resume: stop a worker taking work while someone works
// on its machine, without stopping the service. Run on the worker's host
// they find the worker the way the daemon does (its config, env file and
// key); --worker-id names another.

func workerPauseCmd() *cli.Command {
	return &cli.Command{
		Name:  "pause",
		Usage: "Pause the worker: it takes no new work (a run in progress finishes) until resumed or --for runs out",
		Flags: append(workerTargetFlags(),
			&cli.StringFlag{Name: "for", Usage: "How long, e.g. 90m or 2h (default: until skopos worker resume)"},
			&cli.StringFlag{Name: "reason", Usage: "Shown on the dashboard and in the audit log"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			client, id, err := workerTarget(cmd)
			if err != nil {
				return err
			}
			p, err := client.PauseWorker(ctx, id, strings.TrimSpace(cmd.GetString("for")), cmd.GetString("reason"), "cli")
			if err != nil {
				return err
			}
			until := "until you resume it (skopos worker resume)"
			if !p.Until.IsZero() {
				until = "until " + p.Until.Local().Format("2006-01-02 15:04")
			}
			fmt.Printf("%s paused %s. A run it holds finishes; it takes nothing new.\n", id, until)
			return nil
		},
	}
}

func workerResumeCmd() *cli.Command {
	return &cli.Command{
		Name:  "resume",
		Usage: "Resume a paused worker",
		Flags: workerTargetFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			client, id, err := workerTarget(cmd)
			if err != nil {
				return err
			}
			if err := client.ResumeWorker(ctx, id, "cli"); err != nil {
				return err
			}
			fmt.Printf("%s resumed: it takes work again at its next poll.\n", id)
			return nil
		},
	}
}

func workerTargetFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "worker-id", Usage: "The worker (default: this host's worker, as its config names it)"},
		&cli.StringFlag{Name: "env-file", Usage: "Env file to load (default: skopos-worker.env next to the config)", EnvVars: []string{"SKOPOS_WORKER_ENV"}},
	}
}

// workerTarget resolves the server, key and worker id as the daemon does:
// flag, environment (the env file included), then the config file; the id
// defaults to worker-<hostname>. The config is the one --config names, else
// skopos-config.toml here, else in ~/.config/skopos (where install puts a
// worker's).
func workerTarget(cmd *cli.Command) (*worker.Client, string, error) {
	cfgPath := configFileFor(cmd)
	if _, err := os.Stat(cfgPath); err != nil && !filepath.IsAbs(cfgPath) {
		if global := filepath.Join(GlobalConfigPath(), cfgPath); fileExists(global) {
			cfgPath = global
		}
	}
	if _, _, err := loadWorkerEnvFile(defaultWorkerEnvFile(cfgPath, cmd.GetString("env-file"))); err != nil {
		return nil, "", err
	}
	if err := srvurl.SetDNSServer(os.Getenv("SKOPOS_DNS_SERVER")); err != nil {
		return nil, "", err
	}
	clientCfg, err := readClientSection(cfgPath)
	if err != nil {
		return nil, "", err
	}
	key := resolveClientSetting(cmd.GetString("api-key"), "SKOPOS_API_KEY", "", clientCfg.APIKey)
	if key == "" {
		return nil, "", fmt.Errorf("no API key: set SKOPOS_API_KEY or run this where the worker's env file is")
	}
	server := resolveClientSetting(cmd.GetString("server-url"), "SKOPOS_SERVER_URL", "http://localhost:8080", clientCfg.ServerURL)
	id := resolveWorkerSetting(cmd.GetString("worker-id"), "SKOPOS_WORKER_ID", "")
	if id == "" {
		var file struct {
			Worker struct {
				ID string `toml:"id"`
			} `toml:"worker"`
		}
		if _, err := os.Stat(cfgPath); err == nil {
			if _, err := toml.DecodeFile(cfgPath, &file); err != nil {
				return nil, "", fmt.Errorf("parsing %s: %w", cfgPath, err)
			}
		}
		id = file.Worker.ID
	}
	if id == "" {
		h, _ := os.Hostname()
		id = "worker-" + h
	}
	home, _ := os.UserHomeDir()
	c := worker.NewClient(expandTilde(server, home), key)
	return c, id, nil
}
