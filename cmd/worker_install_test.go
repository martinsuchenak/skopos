package cmd

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The launchd plist must be valid XML and carry the daemon invocation,
// keep-alive, the captured PATH, and the state-dir log.
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
		"<key>RunAtLoad</key>",
		"/var/lib/skopos-worker/service.log",
		"/opt/homebrew/bin:/usr/bin:/bin",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q:\n%s", want, p)
		}
	}
}

// The systemd unit: quoted ExecStart (paths may contain spaces), restart
// policy, captured PATH, and the right install target per mode.
func TestSystemdUnit(t *testing.T) {
	for _, tc := range []struct {
		system bool
		target string
	}{
		{false, "default.target"},
		{true, "multi-user.target"},
	} {
		u := systemdUnit("skopos-worker", "/opt/bin/skopos", "/home/some dir/skopos-config.toml", "/usr/bin:/bin", tc.system)
		for _, want := range []string{
			`ExecStart="/opt/bin/skopos" worker --config "/home/some dir/skopos-config.toml"`,
			"WorkingDirectory=/home/some dir",
			"Restart=always",
			"Environment=PATH=/usr/bin:/bin",
			"WantedBy=" + tc.target,
		} {
			if !strings.Contains(u, want) {
				t.Errorf("unit (system=%v) missing %q:\n%s", tc.system, want, u)
			}
		}
	}
}

// Dry-run writes nothing — only actions.
func TestInstallDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "skopos-config.toml")

	actions, err := installLaunchd(installOptions{label: "w", dryRun: true}, home, "/bin/skopos", cfg, filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 || !strings.Contains(actions[0], "would write") {
		t.Fatalf("expected would-write action, got %v", actions)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "w.plist")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write the plist (err=%v)", err)
	}

	actions, err = installSystemd(installOptions{label: "w", dryRun: true}, home, "/bin/skopos", cfg, filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 || !strings.Contains(actions[0], "would write") {
		t.Fatalf("expected would-write action, got %v", actions)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/systemd/user/w.service")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write the unit (err=%v)", err)
	}
}
