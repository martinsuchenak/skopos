package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The env file feeds secrets to service installs; the real environment always
// wins so a shell override beats the file.
func TestLoadWorkerEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "skopos-worker.env")
	content := "# comment\n\nSKOPOS_TEST_WORKER_KEY=sk_from_file\nSKOPOS_TEST_PUSH_TOKEN=tok\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKOPOS_TEST_PUSH_TOKEN", "tok_from_env")
	defer os.Unsetenv("SKOPOS_TEST_WORKER_KEY")

	if err := loadWorkerEnvFile(envPath); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("SKOPOS_TEST_WORKER_KEY"); got != "sk_from_file" {
		t.Fatalf("file value must apply, got %q", got)
	}
	if got := os.Getenv("SKOPOS_TEST_PUSH_TOKEN"); got != "tok_from_env" {
		t.Fatalf("existing environment must win, got %q", got)
	}

	// Garbage is an error, not a silent skip.
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("NO_EQUALS_SIGN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadWorkerEnvFile(bad); err == nil || !strings.Contains(err.Error(), "NO_EQUALS_SIGN") {
		t.Fatalf("malformed line must error: %v", err)
	}
	if err := loadWorkerEnvFile(""); err != nil {
		t.Fatalf("empty path is a no-op: %v", err)
	}
	if err := loadWorkerEnvFile(filepath.Join(dir, "absent.env")); err == nil || !strings.Contains(err.Error(), "reading env file") {
		t.Fatalf("explicit-but-absent env file must error: %v", err)
	}
}
