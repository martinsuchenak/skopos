package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/worker"
)

// worker init on a fresh directory: an annotated config (0600) that actually
// parses into a worker.Config, plus the secrets env file beside it.
func TestWorkerInitCreatesConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "https://skopos.example.com", []string{"github.com/you/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if st := statMode(cfgPath, 0); st != 0o600 {
		t.Fatalf("config must be 0600, got %o", st)
	}
	var file struct {
		Client map[string]string
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(string(raw), &file); err != nil {
		t.Fatalf("generated config must parse: %v\n%s", err, raw)
	}
	if file.Worker == nil {
		t.Fatal("generated config has no [worker] section")
	}
	if file.Worker.ID == "" || !strings.HasPrefix(file.Worker.ID, "worker-") {
		t.Fatalf("worker id must be prefilled with the worker- prefix: %q", file.Worker.ID)
	}
	if err := file.Worker.Default(t.TempDir()); err != nil {
		t.Fatalf("generated [worker] section must pass Default(): %v", err)
	}
	if !strings.Contains(string(raw), "github.com/you/repo") {
		t.Fatal("workspace blocks must be emitted")
	}
	// The env file sits where the daemon's default --env-file finds it.
	envPath := filepath.Join(dir, "skopos-worker.env")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("env file missing: %v", err)
	}
	if st := statMode(envPath, 0); st != 0o600 {
		t.Fatalf("env file must be 0600, got %o", st)
	}
}

// worker init on an existing config (e.g. from `skopos setup`): only the
// [worker] section is added, everything else preserved; a re-run refuses.
func TestWorkerInitAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	existing := "[client]\nserver_url = \"https://skopos.example.com\"\n\n[server]\nport = 8080\n"
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "", nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"[client]", "skopos.example.com", "[server]", "port = 8080", "[worker]"} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("preserved/generated content missing %q:\n%s", keep, raw)
		}
	}
	var file struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(string(raw), &file); err != nil {
		t.Fatalf("merged config must parse: %v\n%s", err, raw)
	}
	if file.Worker == nil {
		t.Fatal("[worker] missing after append")
	}
	// Re-run: the section holds operator edits by now — refuse.
	if err := runWorkerInit(&out, cfgPath, "", nil); err == nil || !strings.Contains(err.Error(), "already has a [worker] section") {
		t.Fatalf("re-run must refuse: %v", err)
	}
}

// The env file holds secrets once touched — init never overwrites it.
func TestWorkerInitKeepsExistingEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	envPath := filepath.Join(dir, "skopos-worker.env")
	secret := "SKOPOS_API_KEY=sk_real\n"
	if err := os.WriteFile(envPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "", nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil || string(raw) != secret {
		t.Fatalf("env file must be untouched, got %q err %v", raw, err)
	}
}
