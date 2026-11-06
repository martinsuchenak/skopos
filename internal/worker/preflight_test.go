package worker

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failing preflight reports its workspace unhealthy until a later run
// passes; between runs the last result stands; repos without a preflight
// are always healthy.
func TestCheckHealth(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Workspaces: []string{"ws-a", "ws-b"},
		Repos: map[string]*Repo{
			"ws-a": {Path: dir, Preflight: []string{"test -f ok || { echo redis says no; exit 1; }"}},
			"ws-b": {Path: dir},
		},
	}
	w := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := w.checkHealth(t.Context())
	if len(got) != 1 || !strings.Contains(got["ws-a"], "redis says no") {
		t.Fatalf("failing preflight: %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.checkHealth(t.Context()); got["ws-a"] == "" {
		t.Fatalf("the last result stands until the interval passes: %v", got)
	}
	r := w.health["ws-a"]
	r.at = r.at.Add(-defaultPreflightInterval)
	w.health["ws-a"] = r
	if got := w.checkHealth(t.Context()); len(got) != 0 {
		t.Fatalf("a passing preflight makes it healthy again: %v", got)
	}
	// Bounded: a hung preflight is a failure, not a hung worker.
	cfg.Repos["ws-b"].Preflight = []string{"sleep 30"}
	old := preflightTimeoutForTest(300 * time.Millisecond)
	defer old()
	if got := w.checkHealth(t.Context()); !strings.Contains(got["ws-b"], "timed out") {
		t.Fatalf("hung preflight: %v", got)
	}
}

// preflightTimeoutForTest shortens the bound; the returned func restores it.
func preflightTimeoutForTest(d time.Duration) func() {
	old := preflightTimeout
	preflightTimeout = d
	return func() { preflightTimeout = old }
}
