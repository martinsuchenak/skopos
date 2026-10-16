package cmd

import (
	"github.com/martinsuchenak/skopos/internal/worker"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The env file feeds secrets to service installs; the real environment always
// wins so a shell override beats the file. Dotenv forms are accepted.
func TestLoadWorkerEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "skopos-worker.env")
	content := "# comment\n\nSKOPOS_TEST_WORKER_KEY=sk_from_file\nSKOPOS_TEST_PUSH_TOKEN=tok\nexport SKOPOS_TEST_EXPORTED=yes\nSKOPOS_TEST_QUOTED=\"quoted value\"\nSKOPOS_TEST_SPACED =  spaced \n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKOPOS_TEST_PUSH_TOKEN", "tok_from_env")
	defer os.Unsetenv("SKOPOS_TEST_WORKER_KEY")
	defer os.Unsetenv("SKOPOS_TEST_EXPORTED")
	defer os.Unsetenv("SKOPOS_TEST_QUOTED")
	defer os.Unsetenv("SKOPOS_TEST_SPACED")

	keys, warnings, err := loadWorkerEnvFile(envPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("SKOPOS_TEST_WORKER_KEY"); got != "sk_from_file" {
		t.Fatalf("file value must apply, got %q", got)
	}
	if got := os.Getenv("SKOPOS_TEST_PUSH_TOKEN"); got != "tok_from_env" {
		t.Fatalf("existing environment must win, got %q", got)
	}
	if got := os.Getenv("SKOPOS_TEST_EXPORTED"); got != "yes" {
		t.Fatalf("export prefix must be accepted, got %q", got)
	}
	if got := os.Getenv("SKOPOS_TEST_QUOTED"); got != "quoted value" {
		t.Fatalf("quotes must be stripped, got %q", got)
	}
	if got := os.Getenv("SKOPOS_TEST_SPACED"); got != "spaced" {
		t.Fatalf("spaces must be trimmed, got %q", got)
	}
	// Only the keys the FILE provided are reported (the real-env override is
	// not a file key) — the scrubber drops exactly these from agent runs.
	wantKeys := []string{"SKOPOS_TEST_WORKER_KEY", "SKOPOS_TEST_EXPORTED", "SKOPOS_TEST_QUOTED", "SKOPOS_TEST_SPACED"}
	if len(keys) != len(wantKeys) {
		t.Fatalf("file keys %v, want %v", keys, wantKeys)
	}
	for _, k := range wantKeys {
		found := false
		for _, got := range keys {
			if got == k {
				found = true
			}
		}
		if !found {
			t.Errorf("file key %s missing from %v", k, keys)
		}
	}
	if len(warnings) != 0 {
		t.Fatalf("0600 file must not warn: %v", warnings)
	}

	// A group-readable env file warns (it holds secrets).
	loose := filepath.Join(dir, "loose.env")
	if err := os.WriteFile(loose, []byte("A=b\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, warnings, err = loadWorkerEnvFile(loose)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "group/world readable") {
		t.Fatalf("expected readability warning, got %v", warnings)
	}

	// Garbage is an error, not a silent skip.
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("NO_EQUALS_SIGN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadWorkerEnvFile(bad); err == nil || !strings.Contains(err.Error(), "NO_EQUALS_SIGN") {
		t.Fatalf("malformed line must error: %v", err)
	}
	if _, _, err := loadWorkerEnvFile(""); err != nil {
		t.Fatalf("empty path is a no-op: %v", err)
	}
	if _, _, err := loadWorkerEnvFile(filepath.Join(dir, "absent.env")); err == nil || !strings.Contains(err.Error(), "reading env file") {
		t.Fatalf("explicit-but-absent env file must error: %v", err)
	}
}

// H1: values the env file provides win over flag defaults when no explicit
// flag was given — the flag's own env resolution happened at parse time,
// before the file was loaded.
func TestResolveWorkerSetting(t *testing.T) {
	t.Setenv("SKOPOS_TEST_RESOLVE", "from-env")
	if got := resolveWorkerSetting("", "SKOPOS_TEST_RESOLVE", ""); got != "from-env" {
		t.Fatalf("env must fill an unset flag: %q", got)
	}
	if got := resolveWorkerSetting("from-flag", "SKOPOS_TEST_RESOLVE", ""); got != "from-flag" {
		t.Fatalf("explicit flag must win: %q", got)
	}
	// A flag value equal to its default counts as unset — the env (which
	// includes the env file by then) wins.
	if got := resolveWorkerSetting("http://localhost:8080", "SKOPOS_TEST_RESOLVE", "http://localhost:8080"); got != "from-env" {
		t.Fatalf("default-valued flag must lose to env: %q", got)
	}
	if got := resolveWorkerSetting("", "SKOPOS_TEST_RESOLVE_UNSET", "def"); got != "def" {
		t.Fatalf("default applies when nothing else set: %q", got)
	}
}

// Every [worker] field in the file reaches the daemon's config — the
// loader used a field whitelist that silently dropped profiles,
// runner_config and mcp_config (the end-to-end run caught it). Reflection
// keeps this honest for fields added later: a TOML setting every field must
// leave no exported Config field at its zero value. Flags still win.
func TestLoadWorkerConfigFileKeepsEveryField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "skopos-config.toml")
	toml := `[worker]
id = "worker-file"
state_dir = "/var/state"
runners = ["claude", "oc"]
workspaces = ["ws-file"]
poll_every = "7s"
claude_bin = "/opt/claude"
plan_timeout = "3m"
impl_timeout = "9m"
mcp_deny = ["mcp__x__w"]
mcp_servers = ["mcp__skopos"]
mcp_config = "/etc/mcp.json"

[worker.runner_config.oc]
kind = "opencode"

[worker.profiles.php.implement]
wrapper = ["env"]
tools = ["Bash(php:*)"]

[worker.repos."ws-file"]
path = "/clones/ws"

[[worker.accounts]]
name = "a"
runner = "oc"
`
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &worker.Config{StateDir: defaultStateDir}
	if err := loadWorkerConfigFile(cfg, p); err != nil {
		t.Fatal(err)
	}
	v := reflect.ValueOf(*cfg)
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.IsExported() && v.Field(i).IsZero() {
			t.Errorf("worker.Config.%s from the file did not reach the daemon config", f.Name)
		}
	}
	if cfg.ID != "worker-file" || cfg.StateDir != "/var/state" || cfg.Workspaces[0] != "ws-file" {
		t.Fatalf("file values: %+v", cfg)
	}
	// Flags win where given.
	flagged := &worker.Config{ID: "worker-flag", StateDir: "/flag/state", Workspaces: []string{"ws-flag"}}
	if err := loadWorkerConfigFile(flagged, p); err != nil {
		t.Fatal(err)
	}
	if flagged.ID != "worker-flag" || flagged.StateDir != "/flag/state" || flagged.Workspaces[0] != "ws-flag" || flagged.MCPConfig != "/etc/mcp.json" {
		t.Fatalf("flag precedence: %+v", flagged)
	}
}
