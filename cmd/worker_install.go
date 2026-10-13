package cmd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/paularlott/cli"
)

// workerInstallCmd installs `skopos worker` as a platform service: a launchd
// LaunchAgent on macOS, a systemd user unit on Linux (—system for a system
// unit, run as a non-root user). The unit carries no secrets — the daemon
// loads skopos-worker.env itself. PATH is captured from the installing shell
// so the agent CLI (claude, ...) is found inside the service. The config is
// checked first; a config that isn't ready gets the unit written but NOT
// started (a placeholder config would otherwise restart-loop and claim runs
// it cannot execute).
func workerInstallCmd() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "Install the worker as a service (launchd on macOS, systemd on Linux); starts it only when the config passes 'worker check'",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "label", DefaultValue: "skopos-worker", Usage: "Service name (launchd label / systemd unit prefix)"},
			&cli.BoolFlag{Name: "dry-run", Usage: "Print what would be done, write and execute nothing"},
			&cli.BoolFlag{Name: "no-start", Usage: "Write the service definition without enabling/starting it"},
			&cli.BoolFlag{Name: "system", Usage: "Linux: install a system unit under /etc/systemd/system (runs as a non-root user; requires root to install)"},
			&cli.StringFlag{Name: "user", Usage: "Linux --system: the user the worker runs as (default: $SUDO_USER)"},
			&cli.BoolFlag{Name: "allow-root", Usage: "Linux --system: permit running the worker as root (agents get root too — not recommended)"},
			&cli.StringFlag{Name: "path", Usage: "PATH the service gets (default: this shell's; under --system the target user's login PATH)"},
			&cli.StringFlag{Name: "env-file", Usage: "Env file the checks load (default: skopos-worker.env next to the config)", EnvVars: []string{"SKOPOS_WORKER_ENV"}},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkerInstall(os.Stdout, installOptions{
				label:     cmd.GetString("label"),
				dryRun:    cmd.GetBool("dry-run"),
				noStart:   cmd.GetBool("no-start"),
				system:    cmd.GetBool("system"),
				allowRoot: cmd.GetBool("allow-root"),
				user:      cmd.GetString("user"),
				path:      cmd.GetString("path"),
				envFile:   cmd.GetString("env-file"),
				config:    configFileFor(cmd),
			})
		},
	}
}

// workerCheckCmd validates that the config is startable: the key resolves,
// repo paths are real git clones, no placeholder values remain, and the agent
// CLI is findable. Install runs the same checks before starting.
func workerCheckCmd() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "Validate the worker config: key, repo clones, accounts, agent CLI — what install checks before starting",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "env-file", Usage: "Env file to load (default: skopos-worker.env next to the config)", EnvVars: []string{"SKOPOS_WORKER_ENV"}},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			cfgPath := configFileFor(cmd)
			home, _ := os.UserHomeDir()
			problems, err := runWorkerChecks(cfgPath, defaultWorkerEnvFile(cfgPath, cmd.GetString("env-file")), os.Getenv("PATH"), home)
			if err != nil {
				return err
			}
			if len(problems) == 0 {
				fmt.Println("config OK — the worker can start")
				return nil
			}
			fmt.Println("config not ready:")
			for _, p := range problems {
				fmt.Println("  - " + p)
			}
			return fmt.Errorf("%d problem(s) — fix them before starting the worker", len(problems))
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
	label     string
	dryRun    bool
	noStart   bool
	system    bool
	allowRoot bool
	user      string
	path      string
	envFile   string
	config    string
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
	exe, exeWarns, err := resolveWorkerExe()
	if err != nil {
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
	// same way the daemon will (including ~ expansion, review H2).
	home, _ := os.UserHomeDir()
	cfg := &worker.Config{}
	if err := loadWorkerConfigFile(cfg, o.config); err != nil {
		return err
	}
	if err := cfg.Default(home); err != nil {
		return err
	}

	// N4: the service's PATH must be one the TARGET context owns — under
	// sudo that is not this process's PATH (secure_path). An explicit
	// --path wins; --system resolves the target user's login PATH.
	userGroup := [2]string{"", ""}
	if o.system && runtime.GOOS == "linux" {
		userGroup, err = resolveSystemUser(o)
		if err != nil {
			return err
		}
	}
	pathEnv := o.path
	if pathEnv == "" {
		pathEnv = os.Getenv("PATH")
		if userGroup[0] != "" {
			if p, perr := userLoginPath(userGroup[0]); perr == nil {
				pathEnv = p
			} else {
				fmt.Fprintf(out, "NOTE: could not resolve %s's login PATH (%v) — using this shell's; pass --path if the agent CLI lives elsewhere\n", userGroup[0], perr)
			}
		}
	}
	problems, err := runWorkerChecks(o.config, defaultWorkerEnvFile(o.config, o.envFile), pathEnv, home)
	if err != nil {
		return err
	}

	var actions []string
	actions = append(actions, exeWarns...) // N5: not replaced below
	var platformActions []string
	if runtime.GOOS == "darwin" {
		platformActions, err = installLaunchd(o, home, exe, configPath, cfg.StateDir, pathEnv, problems)
	} else {
		platformActions, err = installSystemd(o, home, exe, configPath, cfg.StateDir, pathEnv, userGroup, problems)
	}
	actions = append(actions, platformActions...)
	if err != nil {
		return err
	}
	for _, a := range actions {
		fmt.Fprintln(out, a)
	}
	if len(problems) > 0 {
		fmt.Fprint(out, "\nConfig problems (skopos worker check):\n")
		for _, p := range problems {
			fmt.Fprintln(out, "  - "+p)
		}
		fmt.Fprint(out, "\nNOT STARTED — fix the above, verify with 'skopos worker check', then start (a previously installed service may still be RUNNING the old definition):\n")
		if runtime.GOOS == "darwin" {
			fmt.Fprintf(out, "  launchctl bootstrap gui/%s %s\n", strconv.Itoa(os.Getuid()), filepath.Join(home, "Library", "LaunchAgents", o.label+".plist"))
		} else if o.system {
			fmt.Fprintf(out, "  systemctl enable --now %s\n", o.label)
		} else {
			fmt.Fprintf(out, "  systemctl --user enable --now %s\n", o.label)
		}
		return nil
	}
	restart := "systemctl --user restart " + o.label
	if runtime.GOOS == "darwin" {
		restart = "launchctl kickstart -k gui/" + strconv.Itoa(os.Getuid()) + "/" + o.label
	} else if o.system {
		restart = "systemctl restart " + o.label
	}
	fmt.Fprintf(out, "\nNext steps:\n  - edit the config or skopos-worker.env, then restart: %s\n  - logs: state-dir/service.log (launchd) or journalctl -u %s (systemd)\n  - queue an item and watch it walk queued -> planning -> awaiting_approval\n", restart, o.label)
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

// resolveWorkerExe picks the binary path for the service. A stable wrapper on
// PATH (homebrew's /opt/homebrew/bin/skopos) beats os.Executable's resolved
// path: the versioned Caskroom/Cellar directory dies on the next upgrade and
// launchd would retry a dead path forever (review H3).
func resolveWorkerExe() (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	if lp, lerr := exec.LookPath("skopos"); lerr == nil {
		if same, serr := sameFile(lp, exe); serr == nil && same {
			exe = lp
		}
	}
	var warns []string
	if strings.Contains(exe, "go-build") || strings.HasPrefix(exe, os.TempDir()) {
		warns = append(warns, fmt.Sprintf("NOTE: %s looks like a temporary build — put the binary somewhere stable before relying on the service", exe))
	}
	return exe, warns, nil
}

func sameFile(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}

// placeholderish catches template values that were never filled in.
func placeholderish(v string) bool {
	return strings.ContainsAny(v, "<>") || strings.Contains(v, "...") || strings.Contains(v, "example.com")
}

// runWorkerChecks validates that the config can actually run: the key
// resolves (after the env file), repo paths are git clones, no placeholders
// remain, an account exists, and the agent CLI is on PATH. It mirrors the
// daemon's resolution order exactly.
func runWorkerChecks(configPath, envFile, pathEnv, home string) ([]string, error) {
	// Same env-file side effects as the daemon, so the key check agrees.
	if envFile != "" {
		if _, _, err := loadWorkerEnvFile(envFile); err != nil {
			return nil, err
		}
	}
	var file struct {
		Client struct {
			APIKey    string `toml:"api_key"`
			ServerURL string `toml:"server_url"`
		}
		Worker *worker.Config
	}
	if _, err := toml.DecodeFile(configPath, &file); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", configPath, err)
	}
	var problems []string

	// The SAME precedence the daemon uses (review N1): environment (which
	// now includes the env file), then [client].
	key := resolveClientSetting("", "SKOPOS_API_KEY", "", file.Client.APIKey)
	if key == "" {
		problems = append(problems, "no API key: set [client].api_key or SKOPOS_API_KEY in skopos-worker.env")
	} else if placeholderish(key) {
		problems = append(problems, "API key looks like a placeholder")
	}
	serverURL := resolveClientSetting("", "SKOPOS_SERVER_URL", "http://localhost:8080", file.Client.ServerURL)
	if placeholderish(serverURL) {
		problems = append(problems, "server URL looks like a placeholder: "+serverURL)
	}

	ws := file.Worker
	if ws == nil {
		return append(problems, "no [worker] section — run 'skopos worker init'"), nil
	}
	if err := ws.Default(home); err != nil {
		problems = append(problems, err.Error())
	}
	if len(ws.Accounts) == 0 {
		problems = append(problems, "no [[worker.accounts]] configured — every run would fail for lack of a credential")
	}
	for _, a := range ws.Accounts {
		for k, v := range a.Env {
			if placeholderish(v) {
				problems = append(problems, fmt.Sprintf("account %s: env %s looks like a placeholder", a.Name, k))
			} else if v == "" {
				// Default() already expanded ${VAR}; empty means the variable
				// is not set anywhere the daemon would see it.
				problems = append(problems, fmt.Sprintf("account %s: env %s resolved empty — set the ${VAR} it references in skopos-worker.env", a.Name, k))
			}
		}
	}
	for id, r := range ws.Repos {
		if r.Path == "" || placeholderish(r.Path) {
			problems = append(problems, fmt.Sprintf("repo %s: path is not filled in", id))
			continue
		}
		if !fileExists(filepath.Join(r.Path, ".git")) {
			problems = append(problems, fmt.Sprintf("repo %s: %s is not a git clone", id, r.Path))
		}
		if placeholderish(r.Push.RemoteURL) || placeholderish(r.Push.TokenEnv) {
			problems = append(problems, fmt.Sprintf("repo %s: push config still has placeholders", id))
		}
	}
	if ws.Bin == "" {
		ws.Bin = "claude"
	}
	// Look the CLI up on the PATH the SERVICE will have, not the
	// installer's (sudo's secure_path is not the target user's).
	if !onPath(ws.Bin, pathEnv) {
		problems = append(problems, fmt.Sprintf("agent CLI %q not on the service PATH (%s)", ws.Bin, pathEnv))
	}
	return problems, nil
}

// onPath resolves bin within the given PATH only.
func onPath(bin, pathEnv string) bool {
	cmd := exec.Command("sh", "-c", `command -v "$1"`, "sh", bin)
	cmd.Env = []string{"PATH=" + pathEnv}
	return cmd.Run() == nil
}

// userLoginPath resolves a user's login-shell PATH (under sudo, the
// installer's own PATH is secure_path, not the user's).
func userLoginPath(user string) (string, error) {
	outB, err := exec.Command("sudo", "-u", user, "-i", "sh", "-c", "echo $PATH").Output()
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(outB))
	if p == "" {
		return "", fmt.Errorf("empty PATH")
	}
	return p, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// --- launchd (macOS) ---

func installLaunchd(o installOptions, home, exe, configPath, stateDir, pathEnv string, problems []string) ([]string, error) {
	plistPath := filepath.Join(home, "Library", "LaunchAgents", o.label+".plist")
	content := launchdPlist(o.label, exe, configPath, stateDir, pathEnv)
	if o.dryRun {
		actions := []string{
			fmt.Sprintf("would write %s", plistPath),
			"would run: launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + plistPath,
		}
		if len(problems) > 0 {
			actions = append(actions, "would NOT start — config not ready (skopos worker check)")
		}
		return actions, nil
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", plistPath, err)
	}
	actions := []string{"wrote " + plistPath}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if o.noStart {
		return actions, nil
	}
	if len(problems) > 0 {
		return actions, nil // written, deliberately not started
	}
	gui := "gui/" + strconv.Itoa(os.Getuid())
	// bootout first: re-installing over a loaded agent is the common case.
	_ = exec.Command("launchctl", "bootout", gui+"/"+o.label).Run()
	if outB, err := exec.Command("launchctl", "bootstrap", gui, plistPath).CombinedOutput(); err != nil {
		actions = append(actions, fmt.Sprintf("NOTE: launchctl bootstrap failed (%v) — start manually: launchctl bootstrap %s %s", firstOutputLine(outB), gui, plistPath))
	} else {
		actions = append(actions, "started (launchd will keep it alive)")
	}
	actions = append(actions, "NOTE: if the skopos server is on your LOCAL network, the Local Network permission resets with every binary upgrade — see docs/deployment/worker.md")
	return actions, nil
}

func uninstallLaunchd(o installOptions, home string) ([]string, error) {
	plistPath := filepath.Join(home, "Library", "LaunchAgents", o.label+".plist")
	if o.dryRun {
		return []string{fmt.Sprintf("would stop and remove %s", plistPath)}, nil
	}
	gui := "gui/" + strconv.Itoa(os.Getuid())
	var actions []string
	if outB, err := exec.Command("launchctl", "bootout", gui+"/"+o.label).CombinedOutput(); err == nil {
		actions = append(actions, "stopped")
	} else {
		actions = append(actions, "NOTE: bootout: "+firstOutputLine(outB))
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return actions, err
	}
	actions = append(actions, "removed "+plistPath)
	return actions, nil
}

// launchdPlist is the LaunchAgent definition, modeled on the production-proven
// agent-trial plist: KeepAlive with a throttle (a broken config must not
// spin), Background priority, children die via the daemon's own context
// (AbandonProcessGroup). All values are XML-escaped.
func launchdPlist(label, exe, configPath, stateDir, pathEnv string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + esc(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + esc(exe) + `</string>
    <string>worker</string>
    <string>--config</string>
    <string>` + esc(configPath) + `</string>
  </array>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>AbandonProcessGroup</key>
  <true/>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>` + esc(pathEnv) + `</string>
  </dict>
  <key>StandardOutPath</key>
  <string>` + esc(filepath.Join(stateDir, "service.log")) + `</string>
  <key>StandardErrorPath</key>
  <string>` + esc(filepath.Join(stateDir, "service.log")) + `</string>
</dict>
</plist>
`
}

// --- systemd (Linux) ---

func installSystemd(o installOptions, home, exe, configPath, stateDir, pathEnv string, userGroup [2]string, problems []string) ([]string, error) {
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
	content := systemdUnit(o.label, exe, configPath, pathEnv, o.system, userGroup)

	if o.dryRun {
		actions := []string{
			fmt.Sprintf("would write %s", unitPath),
			"would run: " + strings.Join(append(append([]string{}, systemctl...), "daemon-reload"), " "),
		}
		enable := append(append([]string{}, systemctl[1:]...), "enable", "--now", o.label)
		actions = append(actions, "would run: "+systemctl[0]+" "+strings.Join(enable, " "))
		if !o.system {
			actions = append(actions, "would ensure linger is enabled (loginctl enable-linger)")
		}
		if len(problems) > 0 {
			actions = append(actions, "would NOT start — config not ready (skopos worker check)")
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
	if o.noStart || len(problems) > 0 {
		return actions, nil // written, deliberately not started
	}
	if !o.system {
		// A user unit only runs (and survives logout) with linger enabled.
		if err := exec.Command("loginctl", "enable-linger").Run(); err != nil {
			actions = append(actions, "NOTE: could not enable linger — run: sudo loginctl enable-linger $USER (the unit stops when you log out otherwise)")
		}
	}
	if outB, err := exec.Command(systemctl[0], append(append([]string{}, systemctl[1:]...), "daemon-reload")...).CombinedOutput(); err != nil {
		return actions, fmt.Errorf("systemctl daemon-reload: %v: %s", err, firstOutputLine(outB))
	}
	args := append(append([]string{}, systemctl[1:]...), "enable", "--now", o.label)
	if outB, err := exec.Command(systemctl[0], args...).CombinedOutput(); err != nil {
		return actions, fmt.Errorf("systemctl enable --now: %v: %s", err, firstOutputLine(outB))
	}
	actions = append(actions, "enabled and started")
	return actions, nil
}

// resolveSystemUser picks the user/group a --system unit runs as. Default is
// $SUDO_USER (the human who invoked sudo); root is refused unless
// --allow-root — the agents the worker starts inherit it (review M1).
func resolveSystemUser(o installOptions) ([2]string, error) {
	user := o.user
	if user == "" {
		user = os.Getenv("SUDO_USER")
	}
	if user == "" || user == "root" {
		if !o.allowRoot {
			return [2]string{}, fmt.Errorf("refusing to run the worker as root (its agent runs would be root too) — pass --user <name>, or --allow-root to override")
		}
		return [2]string{"", ""}, nil // root: no User=/Group= lines
	}
	outB, err := exec.Command("id", "-gn", user).Output()
	if err != nil {
		return [2]string{}, fmt.Errorf("resolving group for %s: %v", user, err)
	}
	return [2]string{user, strings.TrimSpace(string(outB))}, nil
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

// systemdUnit: the daemon restarts on failure with a start limit (a broken
// config must not spin), PATH from the installing shell so the agent CLI is
// found, secrets via skopos-worker.env through the daemon, never the unit.
// PATH is quoted and %-escaped per systemd's specifier syntax.
func systemdUnit(label, exe, configPath, pathEnv string, system bool, userGroup [2]string) string {
	target := "default.target"
	if system {
		target = "multi-user.target"
	}
	var userLines string
	if userGroup[0] != "" {
		userLines = "User=" + userGroup[0] + "\nGroup=" + userGroup[1] + "\n"
	}
	return `[Unit]
Description=skopos worker (` + label + `)
Wants=network-online.target
After=network-online.target
# Start limits live in [Unit] since systemd 230; in [Service] the interval
# key is ignored and the default 10s window never trips (review N3).
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
` + userLines + `ExecStart="` + exe + `" worker --config "` + configPath + `"
WorkingDirectory=` + filepath.Dir(configPath) + `
Environment="PATH=` + strings.ReplaceAll(pathEnv, "%", "%%") + `"
Restart=on-failure
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
