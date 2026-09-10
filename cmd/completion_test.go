package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the real CLI once per test — completion must be
// verified through the actual executable, since the generated scripts key on
// argv[0] and call back into the binary at completion time.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "skopos")
	out, err := exec.Command("go", "build", "-o", bin, "..").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runCompletion invokes the built binary with argv and captures stdout.
func runCompletion(t *testing.T, bin string, argv ...string) string {
	t.Helper()
	out, err := exec.Command(bin, argv...).Output()
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return string(out)
}

// TestCompletionScripts: `skopos completion <shell>` emits a real, non-trivial
// script for every supported shell; the script drives completion by calling
// back into the hidden dynamic modes.
func TestCompletionScripts(t *testing.T) {
	bin := buildBinary(t)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		script := runCompletion(t, bin, "completion", shell)
		if len(script) < 500 {
			t.Fatalf("%s: suspiciously short script (%d bytes)", shell, len(script))
		}
		if !strings.Contains(script, "completion") {
			t.Errorf("%s: script does not wire completion callbacks", shell)
		}
	}
}

// TestCompletionDynamicCommands: the hidden --command mode (what the script
// calls while completing) lists our top-level commands.
func TestCompletionDynamicCommands(t *testing.T) {
	bin := buildBinary(t)
	out := runCompletion(t, bin, "completion", "bash", "--command=skopos")
	for _, want := range []string{"serve", "report", "blackboard", "plan", "workspace", "index", "search", "symbol", "who-calls", "call-tree", "impact", "outline", "dead-code", "cycles", "branch-diff", "setup", "install", "cleanup", "init"} {
		if !strings.Contains(out, want) {
			t.Errorf("dynamic command completion missing %q (got: %q)", want, out)
		}
	}
}

// TestCompletionDynamicFlags: the hidden --flag mode lists a command's flags.
func TestCompletionDynamicFlags(t *testing.T) {
	bin := buildBinary(t)
	out := runCompletion(t, bin, "completion", "bash", "--flag=skopos index build")
	for _, flag := range []string{"--index-dir", "--workspace", "--branch"} {
		if !strings.Contains(out, flag) {
			t.Errorf("index build flag completion missing %q (got: %q)", flag, out)
		}
	}
	out = runCompletion(t, bin, "completion", "bash", "--flag=skopos search")
	for _, flag := range []string{"--server-url", "--api-key", "--branch", "--json"} {
		if !strings.Contains(out, flag) {
			t.Errorf("search flag completion missing %q (got: %q)", flag, out)
		}
	}
}
