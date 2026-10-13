package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/paularlott/cli"
)

// workerInstallCmd installs `skopos worker` as a platform service: a launchd
// LaunchAgent on macOS, a systemd user unit on Linux (—system for a system
// unit). The unit carries no secrets — the daemon loads skopos-worker.env
// itself. PATH is captured from the installing shell so the agent CLI
// (claude, ...) is found inside the service.
func workerInstallCmd() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "Install the worker as a service (launchd on macOS, systemd on Linux) and start it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "label", DefaultValue: "skopos-worker", Usage: "Service name (launchd label / systemd unit prefix)"},
			&cli.BoolFlag{Name: "dry-run", Usage: "Print what would be done, write and execute nothing"},
			&cli.BoolFlag{Name: "no-start", Usage: "Write the service definition without enabling/starting it"},
			&cli.BoolFlag{Name: "system", Usage: "Linux: install a system unit under /etc/systemd/system (requires root) instead of a user unit"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkerInstall(os.Stdout,
				installOptions{
					label:   cmd.GetString("label"),
					dryRun:  cmd.GetBool("dry-run"),
					noStart: cmd.GetBool("no-start"),
					system:  cmd.GetBool("system"),
					config:  configFileFor(cmd),
				})
		},
	}
}

func workerUninstallCmd() *cli.Command {
	return &cli.Command{
		Name:  "uninstall",
		Usage: "Stop and remove the worker service (config, env file and state are kept)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "label", DefaultValue: "skopos-worker", Usage: "Service name used at install time"},
			&cli.BoolFlag{Name: "dry-run", Usage: "Print what would be done, write and execute nothing"},
			&cli.BoolFlag{Name: "system", Usage: "Linux: remove the system unit instead of the user unit"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkerUninstall(os.Stdout, installOptions{
				label:  cmd.GetString("label"),
				dryRun: cmd.GetBool("dry-run"),
				system: cmd.GetBool("system"),
			})
		},
	}
}

type installOptions struct {
	label   string
	dryRun  bool
	noStart bool
	system  bool
	config  string
}

func runWorkerInstall(out io.Writer, o installOptions) error {
	switch runtime.GOOS {
	case "darwin", "linux":
	case "windows":
		return fmt.Errorf("service install is not supported on windows — run the worker via NSSM or Task Scheduler (see docs/deployment/worker.md)")
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
	if o.label == "" {
		return fmt.Errorf("--label is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	configPath, err := filepath.Abs(o.config)
	if err != nil {
		return err
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("config %s not found — run 'skopos worker init' first", configPath)
	}

	// The service logs into the state dir; resolve it from the config the
	// same way the daemon will.
	home, _ := os.UserHomeDir()
	cfg := &worker.Config{}
	if err := loadWorkerConfigFile(cfg, o.config); err != nil {
		return err
	}
	if err := cfg.Default(home); err != nil {
		return err
	}

	var actions []string
	if runtime.GOOS == "darwin" {
		actions, err = installLaunchd(o, home, exe, configPath, cfg.StateDir)
	} else {
		actions, err = installSystemd(o, home, exe, configPath, cfg.StateDir)
	}
	if err != nil {
		return err
	}
	for _, a := range actions {
		fmt.Fprintln(out, a)
	}
	restart := "systemctl --user restart " + o.label
	if runtime.GOOS == "darwin" {
		restart = "launchctl kickstart -k gui/" + strconv.Itoa(os.Getuid()) + "/" + o.label
	}
	fmt.Fprintf(out, "\nNext steps:\n  - fill in the config (repos, accounts) and skopos-worker.env, then restart: %s\n  - logs: state-dir/service.log (launchd) or journalctl --user -u %s (systemd)\n  - queue an item and watch it walk queued -> planning -> awaiting_approval\n", restart, o.label)
	return nil
}

func runWorkerUninstall(out io.Writer, o installOptions) error {
	home, _ := os.UserHomeDir()
	var actions []string
	var err error
	if runtime.GOOS == "darwin" {
		actions, err = uninstallLaunchd(o, home)
	} else if runtime.GOOS == "linux" {
		actions, err = uninstallSystemd(o, home)
	} else {
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
	if err != nil {
		return err
	}
	for _, a := range actions {
		fmt.Fprintln(out, a)
	}
	fmt.Fprint(out, "Kept (remove manually if you want): skopos-config.toml, skopos-worker.env, the state dir\n")
	return nil
}

// --- launchd (macOS) ---

func installLaunchd(o installOptions, home, exe, configPath, stateDir string) ([]string, error) {
	plistPath := filepath.Join(home, "Library", "LaunchAgents", o.label+".plist")
	content := launchdPlist(o.label, exe, configPath, stateDir, os.Getenv("PATH"))
	var actions []string
	if o.dryRun {
		return []string{
			fmt.Sprintf("would write %s", plistPath),
			"would run: launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + plistPath,
		}, nil
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", plistPath, err)
	}
	actions = append(actions, "wrote "+plistPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if !o.noStart {
		gui := "gui/" + strconv.Itoa(os.Getuid())
		// bootout first: re-installing over a loaded agent is the common case.
		_ = exec.Command("launchctl", "bootout", gui+"/"+o.label).Run()
		if out, err := exec.Command("launchctl", "bootstrap", gui, plistPath).CombinedOutput(); err != nil {
			actions = append(actions, fmt.Sprintf("NOTE: launchctl bootstrap failed (%v) — start manually: launchctl bootstrap %s %s", firstOutputLine(out), gui, plistPath))
		} else {
			actions = append(actions, "started (launchd will keep it alive)")
		}
	}
	actions = append(actions, "NOTE: if the skopos server is on your LOCAL network and the service cannot connect, that is macOS Local Network permission — see docs/deployment/worker.md")
	return actions, nil
}

func uninstallLaunchd(o installOptions, home string) ([]string, error) {
	plistPath := filepath.Join(home, "Library", "LaunchAgents", o.label+".plist")
	var actions []string
	if o.dryRun {
		return []string{fmt.Sprintf("would stop and remove %s", plistPath)}, nil
	}
	gui := "gui/" + strconv.Itoa(os.Getuid())
	if out, err := exec.Command("launchctl", "bootout", gui+"/"+o.label).CombinedOutput(); err == nil {
		actions = append(actions, "stopped")
	} else {
		actions = append(actions, "NOTE: bootout: "+firstOutputLine(out))
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return actions, err
	}
	actions = append(actions, "removed "+plistPath)
	return actions, nil
}

// launchdPlist is the LaunchAgent definition, modeled on the production-proven
// agent-trial plist: KeepAlive (it is a daemon, not a tick), Background
// priority, children die via the daemon's own context (AbandonProcessGroup).
func launchdPlist(label, exe, configPath, stateDir, pathEnv string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + label + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + exe + `</string>
    <string>worker</string>
    <string>--config</string>
    <string>` + configPath + `</string>
  </array>
  <key>KeepAlive</key>
  <true/>
  <key>RunAtLoad</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>AbandonProcessGroup</key>
  <true/>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>` + pathEnv + `</string>
  </dict>
  <key>StandardOutPath</key>
  <string>` + filepath.Join(stateDir, "service.log") + `</string>
  <key>StandardErrorPath</key>
  <string>` + filepath.Join(stateDir, "service.log") + `</string>
</dict>
</plist>
`
}

// --- systemd (Linux) ---

func installSystemd(o installOptions, home, exe, configPath, stateDir string) ([]string, error) {
	var unitPath string
	systemctl := []string{"systemctl", "--user"}
	if o.system {
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf("--system installs into /etc/systemd/system — re-run with sudo, or drop --system for a user unit")
		}
		unitPath = filepath.Join("/etc/systemd/system", o.label+".service")
		systemctl = []string{"systemctl"}
	} else {
		unitPath = filepath.Join(home, ".config/systemd/user", o.label+".service")
	}
	content := systemdUnit(o.label, exe, configPath, os.Getenv("PATH"), o.system)

	if o.dryRun {
		actions := []string{fmt.Sprintf("would write %s", unitPath)}
		actions = append(actions, "would run: "+strings.Join(append(append([]string{}, systemctl...), "enable", "--now", o.label), " "))
		if !o.system {
			actions = append(actions, "would ensure linger is enabled (loginctl enable-linger)")
		}
		return actions, nil
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", unitPath, err)
	}
	actions := []string{"wrote " + unitPath}
	if !o.system {
		// A user unit only runs (and survives logout) with linger enabled.
		if err := exec.Command("loginctl", "enable-linger").Run(); err != nil {
			actions = append(actions, "NOTE: could not enable linger — run: sudo loginctl enable-linger $USER (the unit stops when you log out otherwise)")
		}
	}
	if !o.noStart {
		if outB, err := exec.Command(systemctl[0], append(systemctl[1:], "daemon-reload")...).CombinedOutput(); err != nil {
			return actions, fmt.Errorf("systemctl daemon-reload: %v: %s", err, firstOutputLine(outB))
		}
		args := append(append([]string{}, systemctl[1:]...), "enable", "--now", o.label)
		if outB, err := exec.Command(systemctl[0], args...).CombinedOutput(); err != nil {
			return actions, fmt.Errorf("systemctl enable --now: %v: %s", err, firstOutputLine(outB))
		}
		actions = append(actions, "enabled and started")
	}
	return actions, nil
}

func uninstallSystemd(o installOptions, home string) ([]string, error) {
	systemctl := []string{"systemctl", "--user"}
	unitPath := filepath.Join(home, ".config/systemd/user", o.label+".service")
	if o.system {
		systemctl = []string{"systemctl"}
		unitPath = filepath.Join("/etc/systemd/system", o.label+".service")
	}
	if o.dryRun {
		return []string{fmt.Sprintf("would stop and remove %s", unitPath)}, nil
	}
	var actions []string
	args := append(append([]string{}, systemctl[1:]...), "disable", "--now", o.label)
	if outB, err := exec.Command(systemctl[0], args...).CombinedOutput(); err != nil {
		actions = append(actions, "NOTE: disable: "+firstOutputLine(outB))
	} else {
		actions = append(actions, "stopped and disabled")
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return actions, err
	}
	actions = append(actions, "removed "+unitPath)
	_ = exec.Command(systemctl[0], append(append([]string{}, systemctl[1:]...), "daemon-reload")...).Run()
	return actions, nil
}

// systemdUnit: the daemon restarts on failure; PATH from the installing shell
// so the agent CLI is found; secrets come from skopos-worker.env via the
// daemon itself, never from the unit.
func systemdUnit(label, exe, configPath, pathEnv string, system bool) string {
	target := "default.target"
	if system {
		target = "multi-user.target"
	}
	return `[Unit]
Description=skopos worker (` + label + `)
Wants=network-online.target
After=network-online.target

[Service]
	ExecStart="` + exe + `" worker --config "` + configPath + `"
WorkingDirectory=` + filepath.Dir(configPath) + `
Environment=PATH=` + pathEnv + `
Restart=always
RestartSec=5

[Install]
WantedBy=` + target + `
`
}

func firstOutputLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
