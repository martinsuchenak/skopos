package cmd

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupCapture(t *testing.T) *strings.Builder {
	t.Helper()
	old := setupOut
	buf := &strings.Builder{}
	setupOut = buf
	t.Cleanup(func() { setupOut = old })
	return buf
}

func TestSetupLocalFlow(t *testing.T) {
	out := setupCapture(t)
	repo := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })
	os.WriteFile("app.go", []byte("package main\n\nfunc Alpha() {}\n"), 0o644)

	// Scripted answers: mode=1 (local), workspace (no git remote here), dir default.
	in := bufio.NewReader(strings.NewReader("1\nmy-ws\nindexes\n"))
	if err := runSetup(context.Background(), in, t.TempDir()); err != nil {
		t.Fatalf("setup: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Indexed 1 files (1 symbols)") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join("indexes", "my-ws.db")); err != nil {
		t.Fatalf("index db missing: %v", err)
	}
}

func TestSetupRemoteFlow(t *testing.T) {
	out := setupCapture(t)
	// A skopos-like server: /health ok, /api/sessions ok without auth.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/api/sessions":
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	dir := t.TempDir()
	// Answers: mode=2 (remote), URL, key(empty), index-now=n.
	in := bufio.NewReader(strings.NewReader("2\n" + ts.URL + "\n\nn\n"))
	if err := runSetup(context.Background(), in, dir); err != nil {
		t.Fatalf("setup: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "connection OK") {
		t.Fatalf("missing connection confirmation:\n%s", out.String())
	}

	// The config file exists, is 0600, and holds the client section.
	raw, err := os.ReadFile(filepath.Join(dir, "skopos-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "[client]") || !strings.Contains(content, ts.URL) {
		t.Fatalf("config content:\n%s", content)
	}
	info, _ := os.Stat(filepath.Join(dir, "skopos-config.toml"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms: %v", info.Mode().Perm())
	}
}

func TestSetupRemoteRejectsWrongKey(t *testing.T) {
	out := setupCapture(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/api/sessions":
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	// mode=2, URL, bad key (rejected), then good key, no indexing.
	in := bufio.NewReader(strings.NewReader("2\n" + ts.URL + "\nbad\ngood\nn\n"))
	if err := runSetup(context.Background(), in, t.TempDir()); err != nil {
		t.Fatalf("setup: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "rejected this key") {
		t.Fatalf("expected rejection feedback:\n%s", out.String())
	}
}

func TestWriteClientConfigMergesSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skopos-config.toml")

	// Create fresh: owner-only file with the section.
	if err := writeClientConfig(path, "http://a", "k1"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "server_url = \"http://a\"") {
		t.Fatalf("create:\n%s", raw)
	}

	// Update existing with other sections preserved.
	if err := writeClientConfig(path, "http://b", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	content := string(raw)
	if !strings.Contains(content, "[client]") || !strings.Contains(content, "http://b") {
		t.Fatalf("update:\n%s", content)
	}
	if strings.Contains(content, "http://a") || strings.Contains(content, "k1") {
		t.Fatalf("stale values remain:\n%s", content)
	}

	// Existing foreign sections survive an update.
	os.WriteFile(path, []byte("[server]\nport = 9999\n\n[client]\nserver_url = \"x\"\n\n[log]\nlevel = \"debug\"\n"), 0o644)
	if err := writeClientConfig(path, "http://c", "k9"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	content = string(raw)
	for _, want := range []string{"port = 9999", "[log]", "level = \"debug\"", "http://c", "k9"} {
		if !strings.Contains(content, want) {
			t.Fatalf("merge lost %q:\n%s", want, content)
		}
	}
}

func TestSetupServerWritesConfigTemplate(t *testing.T) {
	out := setupCapture(t)
	dir := t.TempDir()

	// Mode 3, no existing config.
	in := bufio.NewReader(strings.NewReader("3\n"))
	if err := runSetup(context.Background(), in, dir); err != nil {
		t.Fatalf("setup: %v\noutput:\n%s", err, out.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "skopos-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, section := range []string{"[server]", "[database]", "[auth]", "[health]", "[cleanup]", "[codeindex]", "[log]"} {
		if !strings.Contains(content, section) {
			t.Fatalf("template missing %s:\n%s", section, content)
		}
	}
	info, _ := os.Stat(filepath.Join(dir, "skopos-config.toml"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("template perms: %v", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), "skopos serve") {
		t.Fatalf("missing start hint:\n%s", out.String())
	}

	// Re-running leaves the existing file untouched.
	before := content
	in = bufio.NewReader(strings.NewReader("3\n"))
	if err := runSetup(context.Background(), in, dir); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "skopos-config.toml"))
	if string(after) != before {
		t.Fatal("second run must not rewrite an existing config")
	}
}
