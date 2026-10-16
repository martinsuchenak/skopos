package cmd

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The launchd plist must be valid XML (values are escaped), and carry the
// daemon invocation, keep-alive WITH a throttle, the captured PATH, and the
// state-dir log.
func TestLaunchdPlist(t *testing.T) {
	p := launchdPlist("skopos-worker", "/usr/local/bin/skopos", "/etc/skopos/skopos-config.toml", "", "/var/lib/skopos-worker", "/opt/homebrew/bin:/usr/bin:/bin", false)
	if err := xml.Unmarshal([]byte(p), new(any)); err != nil {
		t.Fatalf("plist must be valid XML: %v\n%s", err, p)
	}
	for _, want := range []string{
		"<string>skopos-worker</string>",
		"<string>/usr/local/bin/skopos</string>",
		"<string>worker</string>",
		"<string>--config</string>",
		"<string>/etc/skopos/skopos-config.toml</string>",
		"<key>KeepAlive</key>",
		"<key>ThrottleInterval</key>",
		"<key>RunAtLoad</key>",
		"/var/lib/skopos-worker/service.log",
		"/opt/homebrew/bin:/usr/bin:/bin",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q:\n%s", want, p)
		}
	}
}

// Paths with XML-special characters must not produce a broken plist.
func TestLaunchdPlistEscapes(t *testing.T) {
	p := launchdPlist("w&<x>", "/bin/skopo&s", "/etc/a&b/config.toml", "/etc/a&b/w.env", "/var/log&x", "/bin", true)
	if err := xml.Unmarshal([]byte(p), new(any)); err != nil {
		t.Fatalf("plist with special chars must stay valid XML: %v\n%s", err, p)
	}
	if strings.Contains(p, "&b") && !strings.Contains(p, "&amp;b") {
		t.Errorf("raw & not escaped:\n%s", p)
	}
}

// The systemd unit: quoted ExecStart (paths may contain spaces), quoted and
// %%-escaped PATH, restart limits, and User/Group for system units.
func TestSystemdUnit(t *testing.T) {
	for _, tc := range []struct {
		system    bool
		target    string
		userGroup [2]string
		userLines string
	}{
		{false, "default.target", [2]string{"", ""}, ""},
		{true, "multi-user.target", [2]string{"", ""}, ""},
		{true, "multi-user.target", [2]string{"martin", "staff"}, "User=martin\nGroup=staff\n"},
	} {
		u := systemdUnit("skopos-worker", "/opt/bin/skopos", "/home/some dir/skopos-config.toml", "", "/usr/bin:/bin:100%/x", tc.system, tc.userGroup)
		unitPart, servicePart := splitUnitSections(u)
		for _, want := range []string{
			"StartLimitIntervalSec=300",
			"StartLimitBurst=5",
		} {
			if !strings.Contains(unitPart, want) {
				t.Errorf("[Unit] missing %q:\n%s", want, u)
			}
		}
		if !strings.Contains(u, "WantedBy="+tc.target) {
			t.Errorf("unit missing WantedBy=%s:\n%s", tc.target, u)
		}
		for _, want := range []string{
			`ExecStart="/opt/bin/skopos" worker --config "/home/some dir/skopos-config.toml"`,
			`Environment="PATH=/usr/bin:/bin:100%%/x"`,
			"WorkingDirectory=/home/some dir",
			"Restart=on-failure",
		} {
			if !strings.Contains(servicePart, want) {
				t.Errorf("[Service] missing %q:\n%s", want, u)
			}
		}
		if strings.Contains(servicePart, "StartLimit") {
			t.Errorf("start limits must live in [Unit] since systemd 230:\n%s", u)
		}
		if tc.userLines != "" && !strings.Contains(u, tc.userLines) {
			t.Errorf("unit missing user lines %q:\n%s", tc.userLines, u)
		}
	}
}

// Dry-run writes and executes nothing — only actions.
func TestInstallDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "skopos-config.toml")

	actions, err := installLaunchd(installOptions{label: "w", dryRun: true}, home, "/bin/skopos", cfg, "", filepath.Join(home, "state"), "/usr/bin:/bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 || !strings.Contains(actions[0], "would write") {
		t.Fatalf("expected would-write action, got %v", actions)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "w.plist")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write the plist (err=%v)", err)
	}

	actions, err = installSystemd(installOptions{label: "w", dryRun: true}, home, "/bin/skopos", cfg, "", "/usr/bin:/bin", [2]string{"", ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(actions, "\n")
	if !strings.Contains(joined, "would write") || !strings.Contains(joined, "daemon-reload") {
		t.Fatalf("dry-run must list write + daemon-reload, got %v", actions)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/systemd/user/w.service")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write the unit (err=%v)", err)
	}
}

// An unready config: the unit is written but NOT started, and dry-run says so.
func TestInstallDoesNotStartUnreadyConfig(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "skopos-config.toml")

	actions, err := installLaunchd(installOptions{label: "w"}, home, "/bin/skopos", cfg, "", filepath.Join(home, "state"), "/usr/bin:/bin", []string{"no API key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "w.plist")); err != nil {
		t.Fatalf("unit must still be written: %v", err)
	}
	joined := strings.Join(actions, "\n")
	if strings.Contains(joined, "bootstrap") && !strings.Contains(joined, "would run") {
		t.Fatalf("must not start on failed checks: %v", actions)
	}
}

// The checker: a freshly scaffolded config (placeholders, no clones, no
// accounts) fails; a filled-in one against a real clone passes.
func TestRunWorkerChecks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")

	// A private bin dir with a claude stub: the CLI lookup is deterministic
	// (the runner's real PATH has no claude — this failed v0.11.0's first
	// CI build).
	binDir := t.TempDir()
	// The stub's --help advertises the lockdown flags the check requires.
	stub := "#!/bin/sh\necho '--setting-sources --restricted --strict-mcp-config --mcp-config --tools'\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := binDir + string(os.PathListSeparator) + "/usr/bin:/bin"

	// Scaffolded-but-unedited: placeholders everywhere.
	if err := runWorkerInit(&strings.Builder{}, cfgPath, "https://skopos.example.com", []string{"github.com/you/repo"}); err != nil {
		t.Fatal(err)
	}
	problems, err := runWorkerChecks(cfgPath, "", pathEnv, "/home/tester")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("placeholder config must fail checks")
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"no API key", "path is not filled in", "server URL looks like a placeholder", "account claude-1: env ANTHROPIC_API_KEY resolved empty"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected problem %q, got:\n%s", want, joined)
		}
	}

	// Filled in: a real clone, an account, a key in the env file.
	clone := filepath.Join(dir, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	filled := "[client]\nserver_url = \"https://skopos.real.com\"\n\n[worker]\nid = \"worker-t\"\n\n[worker.repos.\"ws\"]\npath = " + "\"" + clone + "\"" + "\n\n[[worker.accounts]]\nname = \"a\"\nrunner = \"claude\"\nenv = { ANTHROPIC_API_KEY = \"k\" }\n"
	if err := os.WriteFile(cfgPath, []byte(filled), 0o600); err != nil {
		t.Fatal(err)
	}
	// A key exported in the installer's shell is NOT what the service
	// sees — the check must not pass on it (review M9).
	t.Setenv("SKOPOS_API_KEY", "sk_ambient")
	problems, err = runWorkerChecks(cfgPath, "", pathEnv, "/home/tester")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "no API key") || !strings.Contains(problems[0], "service will not see it") {
		t.Fatalf("an ambient-only key must fail the check with a hint, got %v", problems)
	}
	if os.Getenv("SKOPOS_API_KEY") != "sk_ambient" {
		t.Fatal("the check must restore the caller's environment")
	}
	envFile := filepath.Join(dir, "skopos-worker.env")
	if err := os.WriteFile(envFile, []byte("SKOPOS_API_KEY=sk_real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err = runWorkerChecks(cfgPath, envFile, pathEnv, "/home/tester")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("filled config must pass checks (incl. the CLI on the service PATH), got %v", problems)
	}

	// Without the stub on the PATH, the CLI problem is reported.
	problems, err = runWorkerChecks(cfgPath, envFile, "/usr/bin:/bin", "/home/tester")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "not on the service PATH") {
		t.Fatalf("missing CLI on PATH must be the single problem, got %v", problems)
	}
}

// splitUnitSections splits a unit file into its [Unit] and [Service] bodies.
func splitUnitSections(u string) (unit, service string) {
	cur := ""
	for _, l := range strings.Split(u, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = t
			continue
		}
		switch cur {
		case "[Unit]":
			unit += l + "\n"
		case "[Service]":
			service += l + "\n"
		}
	}
	return unit, service
}

// N1: when the config and the env file disagree, the env file wins — the
// daemon and check must resolve identically.
func TestEnvFileBeatsConfigForClient(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	envPath := filepath.Join(dir, "skopos-worker.env")
	if err := os.WriteFile(cfgPath, []byte("[client]\nserver_url = \"https://skopos.config.example\"\napi_key = \"sk_from_config\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("SKOPOS_SERVER_URL=https://skopos.envfile.real\nSKOPOS_API_KEY=sk_from_envfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// t.Setenv restores the host's values afterwards; Unsetenv makes the
	// env file's keys apply (the real environment wins over the file).
	for _, k := range []string{"SKOPOS_SERVER_URL", "SKOPOS_API_KEY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	if _, _, err := loadWorkerEnvFile(envPath); err != nil {
		t.Fatal(err)
	}
	cc, err := readClientSection(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveClientSetting("", "SKOPOS_SERVER_URL", "http://localhost:8080", cc.ServerURL); got != "https://skopos.envfile.real" {
		t.Fatalf("env file must beat config for the URL: %q", got)
	}
	if got := resolveClientSetting("", "SKOPOS_API_KEY", "", cc.APIKey); got != "sk_from_envfile" {
		t.Fatalf("env file must beat config for the key: %q", got)
	}
	// Without the env file, the config applies.
	if got := resolveClientSetting("", "SKOPOS_SERVER_URL_UNSET_X", "http://localhost:8080", cc.ServerURL); got != "https://skopos.config.example" {
		t.Fatalf("config must be the fallback: %q", got)
	}
}

// Round-3 lows: the checker expands ~ against the PASSED home (the target
// user's under sudo --system), and the login-PATH sentinel survives
// profile banners.
func TestCheckUsesProvidedHomeAndLoginPathParsing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	envFile := filepath.Join(dir, "skopos-worker.env")
	if err := os.WriteFile(envFile, []byte("SKOPOS_API_KEY=sk_real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "[client]\nserver_url = \"https://skopos.real.com\"\n\n[worker]\nid = \"worker-t\"\n\n[worker.repos.\"ws\"]\npath = \"~/clones/ws\"\n\n[[worker.accounts]]\nname = \"a\"\nrunner = \"claude\"\nenv = { ANTHROPIC_API_KEY = \"k\" }\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := runWorkerChecks(cfgPath, envFile, "/usr/bin:/bin", "/home/target")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "/home/target/clones/ws") {
		t.Fatalf("checker must expand ~ against the provided home, got:\n%s", joined)
	}

	p, err := extractLoginPath("Welcome to the box (motd)\nnvm banner noise\n__P=/home/u/.local/bin:/usr/bin\n")
	if err != nil || p != "/home/u/.local/bin:/usr/bin" {
		t.Fatalf("sentinel parse: %q err %v", p, err)
	}
	if _, err := extractLoginPath("no marker at all\n"); err == nil {
		t.Fatal("missing marker must error")
	}
}

// An explicit env file reaches the service definition (review H3), and a
// not-started plist is parked Disabled so login does not start it (M10).
func TestServiceCarriesEnvFileAndDisabled(t *testing.T) {
	p := launchdPlist("w", "/bin/skopos", "/etc/s/c.toml", "/etc/s/w.env", "/var/s", "/bin", true)
	if err := xml.Unmarshal([]byte(p), new(any)); err != nil {
		t.Fatalf("plist must be valid XML: %v\n%s", err, p)
	}
	for _, want := range []string{"<string>--env-file</string>", "<string>/etc/s/w.env</string>", "<key>Disabled</key>"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q:\n%s", want, p)
		}
	}
	if p := launchdPlist("w", "/bin/skopos", "/etc/s/c.toml", "", "/var/s", "/bin", false); strings.Contains(p, "Disabled") || strings.Contains(p, "--env-file") {
		t.Errorf("a started plist with the default env file carries neither key:\n%s", p)
	}
	u := systemdUnit("w", "/bin/skopos", "/etc/s/c.toml", "/etc/s/w.env", "/bin", false, [2]string{"", ""})
	if !strings.Contains(u, `ExecStart="/bin/skopos" worker --config "/etc/s/c.toml" --env-file "/etc/s/w.env"`) {
		t.Errorf("unit must pass the env file:\n%s", u)
	}

	home := t.TempDir()
	if _, err := installLaunchd(installOptions{label: "w"}, home, "/bin/skopos", "/etc/s/c.toml", "", filepath.Join(home, "state"), "/bin", []string{"no API key"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "w.plist"))
	if err != nil || !strings.Contains(string(raw), "<key>Disabled</key>") {
		t.Fatalf("an unready install must be written Disabled (err=%v):\n%s", err, raw)
	}
}

// An agent CLI without the lockdown flags fails the check (every run would
// fail on the unknown option); a wrapper missing from the service PATH and
// a missing mcp_config file are reported too.
func TestCheckRequiresLockdownCLIAndWrappers(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\necho 'usage: claude [options]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := binDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	envFile := filepath.Join(dir, "skopos-worker.env")
	if err := os.WriteFile(envFile, []byte("SKOPOS_API_KEY=sk_real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(dir, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[client]\nserver_url = \"https://skopos.real.com\"\n\n[worker]\nid = \"worker-t\"\nmcp_servers = [\"mcp__skopos\"]\nmcp_config = \"" + filepath.Join(dir, "missing-mcp.json") + "\"\n\n" +
		"[worker.profiles.php.implement]\nwrapper = [\"no-such-wrapper-xyz\", \"run\"]\ntools = [\"Bash(php:*)\"]\n\n" +
		"[worker.repos.\"ws\"]\npath = \"" + clone + "\"\nprofile = \"php\"\n\n[[worker.accounts]]\nname = \"a\"\nrunner = \"claude\"\nenv = { ANTHROPIC_API_KEY = \"k\" }\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := runWorkerChecks(cfgPath, envFile, pathEnv, "/home/tester")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"does not support --setting-sources", "mcp_config", "wrapper \"no-such-wrapper-xyz\" not on the service PATH"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected problem %q, got:\n%s", want, joined)
		}
	}
}
