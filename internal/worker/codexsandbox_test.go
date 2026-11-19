package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/policy"
)

// fakeCodex writes a codex stand-in whose `sandbox` command prints out and
// exits with code, and records its arguments, working directory and
// CODEX_HOME in a log next to it.
func fakeCodex(t *testing.T, out string, code int) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "codex"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" \"$PWD\" \"$CODEX_HOME\" \"$(ls -A \"$CODEX_HOME\" | wc -l | tr -d ' ')\" >> " + log + "\n" +
		"printf '%s\\n' '" + out + "' >&2\n" +
		"exit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// The probe starts codex's sandbox with a run's permission profile in an
// empty CODEX_HOME, and turns codex's own words into the reason.
func TestCodexSandboxProblem(t *testing.T) {
	ctx := context.Background()
	bin, log := fakeCodex(t, "", 0)
	if why := codexSandboxProblem(ctx, bin, os.Environ()); why != "" {
		t.Fatalf("a sandbox that starts: %q", why)
	}
	calls, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "sandbox --permission-profile skopos -C ") || !strings.HasSuffix(lines[0], " -- true") ||
		!strings.Contains(lines[0], "permissions.skopos.network.enabled=false") {
		t.Fatalf("the run's profile around true: %q", lines)
	}
	if !strings.HasPrefix(lines[2], lines[1]) || lines[3] != "0" {
		t.Fatalf("an empty CODEX_HOME of its own: %q", lines)
	}
	if _, err := os.Stat(lines[1]); !os.IsNotExist(err) {
		t.Fatalf("the probe's folder is removed: %v", err)
	}

	bin, _ = fakeCodex(t, "bwrap: No permissions to create a new namespace, likely because the kernel does not allow non-privileged user namespaces.", 1)
	if why := codexSandboxProblem(ctx, bin, os.Environ()); !strings.Contains(why, "user namespace") || !strings.Contains(why, "No permissions to create a new namespace") {
		t.Fatalf("the namespace refusal, with a hint: %q", why)
	}
	bin, _ = fakeCodex(t, "Error: sandbox failed", 1)
	if why := codexSandboxProblem(ctx, bin, os.Environ()); why != "codex's sandbox cannot start: Error: sandbox failed" {
		t.Fatalf("any other failure: %q", why)
	}
	bin, _ = fakeCodex(t, "error: unrecognized subcommand 'sandbox'", 2)
	if why := codexSandboxProblem(ctx, bin, os.Environ()); why != "" {
		t.Fatalf("a codex without the command cannot be checked, and is not held: %q", why)
	}
}

// Where codex's sandbox cannot start, the codex runner's runs are held
// back (none:check, with the reason) and every other runner's are not; a
// codex run inside a wrapper is left alone. Fixed, codex runs again.
func TestCodexSandboxHoldsCodexBack(t *testing.T) {
	was := codexSandboxGOOS
	codexSandboxGOOS = "linux"
	t.Cleanup(func() { codexSandboxGOOS = was })
	c := executorConfig(t)
	bin, _ := fakeCodex(t, "bwrap: No permissions to create a new namespace", 1)
	c.RunnerSpecs["codex"].Bin = bin
	w := &Worker{cfg: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	before := c.ExecutorFor("bare", "review", "claude")
	if !w.checkCodexSandbox(ctx) {
		t.Fatal("the check's result is new")
	}
	if got := c.ExecutorFor("bare", "review", "codex"); got != policy.NoExecutor(policy.NoneCheck) {
		t.Fatalf("codex is held back: %s", got)
	}
	if got := c.ExecutorFor("bare", "review", "claude"); got != before {
		t.Fatalf("claude is not: %s, was %s", got, before)
	}
	if got := c.ExecutorFor("ws", "implement", "codex"); !strings.HasPrefix(got, "wrapper:") {
		t.Fatalf("a wrapped codex run is not: %s", got)
	}
	var reason string
	for _, f := range c.ExecutorFacts()["bare"] {
		if f.Executor == policy.NoExecutor(policy.NoneCheck) {
			if f.Runner != "codex" && f.Runner != "" {
				t.Fatalf("only codex's facts are held: %+v", f)
			}
			reason = f.Reason
		}
	}
	if !strings.Contains(reason, "user namespace") {
		t.Fatalf("the fact says why: %q", reason)
	}
	if w.checkCodexSandbox(ctx) {
		t.Fatal("not due again at once")
	}
	held, _ := c.StageCheck(ctx)
	if len(held) != 1 || !strings.HasPrefix(held[0], "runner codex (unless wrapped): codex's sandbox cannot start") {
		t.Fatalf("worker check names it: %q", held)
	}

	fixed, _ := fakeCodex(t, "", 0)
	c.RunnerSpecs["codex"].Bin = fixed
	if !w.checkCodexSandbox(ctx) {
		t.Fatal("a new binary checks again, and the result changed")
	}
	if got := c.ExecutorFor("bare", "review", "codex"); got == policy.NoExecutor(policy.NoneCheck) {
		t.Fatalf("fixed, codex runs again: %s", got)
	}

	codexSandboxGOOS = "darwin"
	if w.checkCodexSandbox(ctx) || len(c.codexBad) != 0 {
		t.Fatal("macOS's sandbox is the system's own: no check")
	}
}
