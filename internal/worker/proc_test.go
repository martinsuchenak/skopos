//go:build unix

package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func alive(t *testing.T, pidFile string) bool {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return syscall.Kill(pid, 0) == nil
}

// A hang seen live: a setup command whose child outlives the timeout and
// keeps the output pipe open. The timeout now stops the whole tree and the
// worker gets its error back.
func TestSetupTimeoutStopsTheProcessTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	start := time.Now()
	_, err := runSetup(t.Context(), dir, []string{"sh -c 'sleep 60 & echo $! > " + pidFile + "; wait'"}, dir, nil, nil, nil, runLimits{total: time.Second})
	if err == nil || !strings.Contains(err.Error(), "setup_timeout") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("runSetup took %s after a 1s timeout", d)
	}
	time.Sleep(100 * time.Millisecond)
	if alive(t, pidFile) {
		t.Fatal("the setup's child survived the timeout")
	}
}

// A command that exits but leaves a background child holding the pipe no
// longer blocks the worker: Wait returns after the wait delay.
func TestBackgroundChildDoesNotBlock(t *testing.T) {
	old := processWaitDelay
	processWaitDelay = 500 * time.Millisecond
	defer func() { processWaitDelay = old }()
	dir := t.TempDir()
	start := time.Now()
	// It exited cleanly, so it succeeded: the child is not its failure.
	if _, err := runShell(t.Context(), dir, nil, "sleep 30 & echo started", 0); err != nil {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("runShell waited %s for a background child", d)
	}
}

// check_timeout bounds the checks together: the one running when it passes
// is stopped and fails, the rest fail without running.
func TestCheckTimeout(t *testing.T) {
	dir := t.TempDir()
	out := runChecks(t.Context(), dir, []string{"true", "sleep 30", "true"}, nil, nil, runLimits{total: time.Second})
	if len(out) != 3 || !out[0].Passed || out[1].Passed || !strings.Contains(out[1].Output, "check_timeout") ||
		out[2].Passed || !strings.Contains(out[2].Output, "not run") {
		t.Fatalf("checks: %+v", out)
	}
}

// quiet_timeout stops a command that prints nothing for that long; one that
// keeps printing runs on. A stopped check is marked as stopped (a hang, not
// a failure for the implementer to fix).
func TestQuietTimeout(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	_, err := runSetup(t.Context(), dir, []string{"echo hi; sleep 30"}, dir, nil, nil, nil, runLimits{total: time.Minute, quiet: 300 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no output for 300ms") {
		t.Fatalf("a silent setup must be stopped as quiet: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the quiet stop took %s", d)
	}
	// Printing every 100ms for ~600ms: never silent for 300ms.
	if _, err := runSetup(t.Context(), dir, []string{"for i in 1 2 3 4 5 6; do echo $i; sleep 0.1; done"}, dir, nil, nil, nil, runLimits{total: time.Minute, quiet: 300 * time.Millisecond}); err != nil {
		t.Fatalf("a command that keeps printing must run on: %v", err)
	}
	out := runChecks(t.Context(), dir, []string{"sleep 30", "false"}, nil, nil, runLimits{total: time.Minute, quiet: 300 * time.Millisecond})
	if len(out) != 2 || out[0].Passed || !out[0].Stopped || !strings.Contains(out[0].Output, "quiet_timeout") || out[1].Stopped {
		t.Fatalf("checks: %+v", out)
	}
	if got := stoppedChecks(out); len(got) != 1 || got[0].Name != "sleep 30" {
		t.Fatalf("stoppedChecks: %+v", got)
	}
}
