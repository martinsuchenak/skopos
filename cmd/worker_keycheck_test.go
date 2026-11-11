package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/worker"
)

// The other tests in this package check configs that name servers which do
// not exist: they must not look them up.
func init() {
	checkWorkerKey = func(context.Context, string, string) ([]string, error) { return nil, nil }
	checkServerConfig = func(context.Context, *worker.Config, string, string) ([]string, []string) { return nil, nil }
}

// `skopos worker check` asks the server about the key: one that holds the
// admin permission is a problem here, not a restart loop after install. A
// plain key passes, and so does a server that cannot be reached.
func TestWorkerCheckRefusesAdminKey(t *testing.T) {
	checkWorkerKey = worker.CheckWorkerKey
	t.Cleanup(func() {
		checkWorkerKey = func(context.Context, string, string) ([]string, error) { return nil, nil }
	})
	answer := `{"root":false,"key":{"approver":false,"admin":true}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/whoami" || r.Header.Get("Authorization") != "Bearer sk_worker-key" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()

	dir := t.TempDir()
	clone := filepath.Join(dir, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	write := func(server string) {
		cfg := "[client]\nserver_url = \"" + server + "\"\napi_key = \"sk_worker-key\"\n\n[worker]\nid = \"worker-t\"\n\n[worker.repos.\"ws\"]\npath = \"" + clone + "\"\n\n[[worker.accounts]]\nname = \"a\"\nrunner = \"claude\"\nenv = { ANTHROPIC_API_KEY = \"k\" }\n"
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adminProblems := func() []string {
		problems, err := runWorkerChecks(cfgPath, "", bin, dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range problems {
			if strings.Contains(p, "admin permission") {
				out = append(out, p)
			}
		}
		return out
	}

	write(srv.URL)
	if got := adminProblems(); len(got) != 1 {
		t.Fatalf("an admin worker key must be a problem: %v", got)
	}
	answer = `{"root":false,"key":{"approver":false,"admin":false}}`
	if got := adminProblems(); len(got) != 0 {
		t.Fatalf("a plain key passes: %v", got)
	}
	write("http://127.0.0.1:1")
	if got := adminProblems(); len(got) != 0 {
		t.Fatalf("an unreachable server is not this check's finding: %v", got)
	}
}
