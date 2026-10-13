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
	p := launchdPlist("skopos-worker", "/usr/local/bin/skopos", "/etc/skopos/skopos-config.toml", "/var/lib/skopos-worker", "/opt/homebrew/bin:/usr/bin:/bin")
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
	p := launchdPlist("w&<x>", "/bin/skopo&s", "/etc/a&b/config.toml", "/var/log&x", "/bin")
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
		u := systemdUnit("skopos-worker", "/opt/bin/skopos", "/home/some dir/skopos-config.toml", "/usr/bin:/bin:100%/x", tc.system, tc.userGroup)
		for _, want := range []string{
			`ExecStart="/opt/bin/skopos" worker --config "/home/some dir/skopos-config.toml"`,
			`Environment="PATH=/usr/bin:/bin:100%%/x"`,
			"WorkingDirectory=/home/some dir",
			"Restart=on-failure",
			"StartLimitBurst=5",
			"WantedBy=" + tc.target,
		} {
			if !strings.Contains(u, want) {
				t.Errorf("unit missing %q:\n%s", want, u)
			}
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

	actions, err := installLaunchd(installOptions{label: "w", dryRun: true}, home, "/bin/skopos", cfg, filepath.Join(home, "state"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 || !strings.Contains(actions[0], "would write") {
		t.Fatalf("expected would-write action, got %v", actions)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "w.plist")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write the plist (err=%v)", err)
	}

	actions, err = installSystemd(installOptions{label: "w", dryRun: true}, home, "/bin/skopos", cfg, filepath.Join(home, "state"), nil)
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

	actions, err := installLaunchd(installOptions{label: "w"}, home, "/bin/skopos", cfg, filepath.Join(home, "state"), []string{"no API key"})
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

	// Scaffolded-but-unedited: placeholders everywhere.
	if err := runWorkerInit(&strings.Builder{}, cfgPath, "https://skopos.example.com", []string{"github.com/you/repo"}); err != nil {
		t.Fatal(err)
	}
	problems, err := runWorkerChecks(cfgPath, os.Getenv("PATH"))
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
	t.Setenv("SKOPOS_API_KEY", "sk_real")
	problems, err = runWorkerChecks(cfgPath, os.Getenv("PATH"))
	if err != nil {
		t.Fatal(err)
	}
	// The agent CLI check is environment-dependent — drop it before asserting.
	var rest []string
	for _, p := range problems {
		if strings.Contains(p, "not on PATH") {
			continue
		}
		rest = append(rest, p)
	}
	if len(rest) != 0 {
		t.Fatalf("filled config must pass checks, got %v", rest)
	}
}
