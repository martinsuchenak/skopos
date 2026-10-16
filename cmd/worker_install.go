package cmd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
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
			&cli.StringFlag{Name: "env-file", Usage: "Env file the service loads — checked first, then baked into the unit (default: skopos-worker.env next to the config)", EnvVars: []string{"SKOPOS_WORKER_ENV"}},
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
	// An explicit env file must reach the SERVICE, not only the checks: the
	// daemon otherwise looks for the default file next to the config, finds
	// no key and restart-loops (review H3). The default needs no flag.
	serviceEnvFile := ""
	if o.envFile != "" {
		if serviceEnvFile, err = filepath.Abs(o.envFile); err != nil {
			return err
		}
		if _, err := os.Stat(serviceEnvFile); err != nil {
			return fmt.Errorf("env file %s not found", serviceEnvFile)
		}
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
	// The checker must expand ~ against the TARGET user's home (under sudo
	// --system, this process's home is /root; the daemon runs as the target
	// user, so only the check would be wrong — in the cautious direction).
	checkHome := home
	if userGroup[0] != "" {
		if u, uerr := user.Lookup(userGroup[0]); uerr == nil && u.HomeDir != "" {
			checkHome = u.HomeDir
		}
	}
	problems, err := runWorkerChecks(o.config, defaultWorkerEnvFile(o.config, serviceEnvFile), pathEnv, checkHome)
	if err != nil {
		return err
	}

	var actions []string
	actions = append(actions, exeWarns...) // N5: not replaced below
	var platformActions []string
	if runtime.GOOS == "darwin" {
		platformActions, err = installLaunchd(o, home, exe, configPath, serviceEnvFile, cfg.StateDir, pathEnv, problems)
	} else {
		platformActions, err = installSystemd(o, home, exe, configPath, serviceEnvFile, pathEnv, userGroup, problems)
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
		_, _ = fmt.Fprint(out, "\nNOT STARTED — the service is written DISABLED (it will not start at login/boot either). Fix the above, verify with 'skopos worker check', then re-run 'skopos worker install' with the same flags. A previously running instance keeps running its old definition until then.\n")
		return nil
	}
	restart := "systemctl --user restart " + o.label
	if runtime.GOOS == "darwin" {
		restart = "launchctl kickstart -k gui/" + strconv.Itoa(os.Getuid()) + "/" + o.label
	} else if o.system {
		restart = "systemctl restart " + o.label
	}
	logs := "state-dir/service.log"
	if runtime.GOOS == "linux" {
		logs = "journalctl --user -u " + o.label
		if o.system {
			logs = "journalctl -u " + o.label
		}
	}
	_, _ = fmt.Fprintf(out, "\nNext steps:\n  - edit the config or the env file, then restart: %s\n  - logs: %s\n  - queue an item and watch it walk queued -> planning -> awaiting_approval\n", restart, logs)
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
	// Check in the environment the SERVICE gets — PATH, HOME and the env
	// file — never the installer's shell: an exported SKOPOS_API_KEY (normal
	// for anyone using skopos in remote mode) made the check pass while the
	// service restart-looped without a key (review M9).
	ambientKey := os.Getenv("SKOPOS_API_KEY") != ""
	restore := isolateEnv(map[string]string{"PATH": pathEnv, "HOME": home})
	defer restore()
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
		msg := "no API key: set [client].api_key or SKOPOS_API_KEY in skopos-worker.env"
		if ambientKey {
			msg += " (SKOPOS_API_KEY is set in this shell, but the service will not see it)"
		}
		problems = append(problems, msg)
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
	} else if err := worker.LockdownSupported(context.Background(), ws.Bin, pathEnv); err != nil {
		problems = append(problems, err.Error())
	}
	if ws.MCPConfig != "" && !fileExists(ws.MCPConfig) {
		problems = append(problems, "mcp_config "+ws.MCPConfig+" does not exist")
	}
	// Profile wrappers must exist where the service runs (a missing docker
	// would fail every contained stage at run time).
	seen := map[string]bool{}
	checkWrapper := func(where string, p *worker.StageProfile) {
		if p == nil || len(p.Wrapper) == 0 || seen[p.Wrapper[0]] {
			return
		}
		seen[p.Wrapper[0]] = true
		if !onPath(p.Wrapper[0], pathEnv) {
			problems = append(problems, fmt.Sprintf("%s: wrapper %q not on the service PATH", where, p.Wrapper[0]))
		}
	}
	for name, stages := range ws.Profiles {
		for stage, p := range stages {
			checkWrapper("profile "+name+"."+stage, p)
		}
	}
	for id, r := range ws.Repos {
		for stage, p := range r.Stages {
			checkWrapper("repo "+id+" stage "+stage, p)
		}
	}
	return problems, nil
}

// isolateEnv replaces the process environment with exactly vars and returns
// a func that restores the original. Only for the single-threaded CLI check.
func isolateEnv(vars map[string]string) func() {
	saved := os.Environ()
	os.Clearenv()
	for k, v := range vars {
		_ = os.Setenv(k, v)
	}
	return func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(k, v)
			}
		}
	}
}

// onPath resolves bin within the given PATH only.
func onPath(bin, pathEnv string) bool {
	cmd := exec.Command("sh", "-c", `command -v "$1"`, "sh", bin)
	cmd.Env = []string{"PATH=" + pathEnv}
	return cmd.Run() == nil
}

// userLoginPath resolves a user's login-shell PATH (under sudo, the
// installer's own PATH is secure_path, not the user's). The sentinel prefix
// survives profiles that echo banners — only the __P= line is the PATH.
func userLoginPath(userName string) (string, error) {
	outB, err := exec.Command("sudo", "-u", userName, "-i", "sh", "-c", `printf '__P=%s\n' "$PATH"`).Output()
	if err != nil {
		return "", err
	}
	return extractLoginPath(string(outB))
}

// extractLoginPath pulls the last __P=-prefixed line out of login-shell
// output (earlier lines may be MOTD or profile banners).
func extractLoginPath(out string) (string, error) {
	p := ""
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), "__P="); ok {
			p = v
		}
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("no __P= marker in login shell output")
	}
	return p, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// --- launchd (macOS) ---

func installLaunchd(o installOptions, home, exe, configPath, envFile, stateDir, pathEnv string, problems []string) ([]string, error) {
	plistPath := filepath.Join(home, "Library", "LaunchAgents", o.label+".plist")
	// Not starting means not starting at the next login either: launchd
	// loads every LaunchAgent at login, and RunAtLoad+KeepAlive would run
	// the unready config then (review M10). Disabled keeps it parked.
	start := !o.noStart && len(problems) == 0
	content := launchdPlist(o.label, exe, configPath, envFile, stateDir, pathEnv, !start)
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
	gui := "gui/" + strconv.Itoa(os.Getuid())
	if !start {
		return append(actions, "written disabled — not started"), nil
	}
	// bootout first: re-installing over a loaded agent is the common case.
	// enable clears a disabled override a previous not-started install (or
	// a manual launchctl disable) left behind.
	_ = exec.Command("launchctl", "bootout", gui+"/"+o.label).Run()
	_ = exec.Command("launchctl", "enable", gui+"/"+o.label).Run()
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
func launchdPlist(label, exe, configPath, envFile, stateDir, pathEnv string, disabled bool) string {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	envArg, disabledKey := "", ""
	if envFile != "" {
		envArg = "\n    <string>--env-file</string>\n    <string>" + esc(envFile) + "</string>"
	}
	if disabled {
		disabledKey = "\n  <key>Disabled</key>\n  <true/>"
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
    <string>` + esc(configPath) + `</string>` + envArg + `
  </array>` + disabledKey + `
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

func installSystemd(o installOptions, home, exe, configPath, envFile, pathEnv string, userGroup [2]string, problems []string) ([]string, error) {
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
	content := systemdUnit(o.label, exe, configPath, envFile, pathEnv, o.system, userGroup)
	sc := func(args ...string) *exec.Cmd {
		return exec.Command(systemctl[0], append(append([]string{}, systemctl[1:]...), args...)...)
	}

	if o.dryRun {
		actions := []string{
			fmt.Sprintf("would write %s", unitPath),
			"would run: " + strings.Join(append(append([]string{}, systemctl...), "daemon-reload"), " "),
		}
		if o.noStart || len(problems) > 0 {
			actions = append(actions, "would run: "+strings.Join(append(append([]string{}, systemctl...), "disable", o.label), " "))
			if len(problems) > 0 {
				actions = append(actions, "would NOT start — config not ready (skopos worker check)")
			}
			return actions, nil
		}
		actions = append(actions, "would run: "+strings.Join(append(append([]string{}, systemctl...), "enable", o.label), " "))
		actions = append(actions, "would run: "+strings.Join(append(append([]string{}, systemctl...), "restart", o.label), " "))
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
	if o.noStart || len(problems) > 0 {
		// Written, deliberately not started — and not left enabled: an
		// enable symlink from an earlier install would start this definition
		// at the next boot (review M10). disable also reloads the manager.
		if outB, err := sc("disable", o.label).CombinedOutput(); err == nil {
			actions = append(actions, "disabled — not started")
		} else if line := firstOutputLine(outB); line != "" {
			actions = append(actions, "NOTE: disable: "+line)
		}
		return actions, nil
	}
	if !o.system {
		// A user unit only runs (and survives logout) with linger enabled.
		if err := exec.Command("loginctl", "enable-linger").Run(); err != nil {
			actions = append(actions, "NOTE: could not enable linger — run: sudo loginctl enable-linger $USER (the unit stops when you log out otherwise)")
		}
	}
	if outB, err := sc("daemon-reload").CombinedOutput(); err != nil {
		return actions, fmt.Errorf("systemctl daemon-reload: %v: %s", err, firstOutputLine(outB))
	}
	if outB, err := sc("enable", o.label).CombinedOutput(); err != nil {
		return actions, fmt.Errorf("systemctl enable: %v: %s", err, firstOutputLine(outB))
	}
	// restart, not `enable --now`: on a re-install the unit is usually
	// active, and --now leaves the old process (old binary, args, PATH)
	// running (review M11). restart starts an inactive unit too.
	if outB, err := sc("restart", o.label).CombinedOutput(); err != nil {
		return actions, fmt.Errorf("systemctl restart: %v: %s", err, firstOutputLine(outB))
	}
	actions = append(actions, "enabled and (re)started")
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
func systemdUnit(label, exe, configPath, envFile, pathEnv string, system bool, userGroup [2]string) string {
	target := "default.target"
	if system {
		target = "multi-user.target"
	}
	envArg := ""
	if envFile != "" {
		envArg = ` --env-file "` + envFile + `"`
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
` + userLines + `ExecStart="` + exe + `" worker --config "` + configPath + `"` + envArg + `
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
