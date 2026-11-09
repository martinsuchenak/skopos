package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paularlott/cli"
)

// skopos worker pause/resume on the worker's host find the worker as the
// daemon does: its config in ~/.config/skopos, the key from the env file
// beside it, the id from [worker].
func TestWorkerPauseFindsTheWorker(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Skopos-Via")+" "+string(body))
		if strings.HasSuffix(r.URL.Path, "/pause") {
			_, _ = w.Write([]byte(`{"at":"2026-11-09T09:00:00Z","until":"2026-11-09T11:00:00Z","by":"key box"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"SKOPOS_API_KEY", "SKOPOS_SERVER_URL", "SKOPOS_WORKER_ID", "SKOPOS_WORKER_ENV"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Chdir(t.TempDir()) // no skopos-config.toml here
	dir := filepath.Join(home, ".config", "skopos")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skopos-config.toml"), []byte("[client]\nserver_url = \""+srv.URL+"\"\n\n[worker]\nid = \"worker-box\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skopos-worker.env"), []byte("SKOPOS_API_KEY=sk_worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		old := os.Args
		defer func() { os.Args = old }()
		os.Args = append([]string{"skopos", "worker"}, args...)
		root := &cli.Command{Name: "skopos", Commands: []*cli.Command{{Name: "worker", Commands: []*cli.Command{workerPauseCmd(), workerResumeCmd()}}}}
		if err := root.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run("pause", "--for", "2h", "--reason", "hands-on")
	run("resume")
	want := []string{
		`POST /api/workers/worker-box/pause Bearer sk_worker cli {"for":"2h","reason":"hands-on"}`,
		`POST /api/workers/worker-box/resume Bearer sk_worker cli `,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s", strings.Join(got, "\n"))
	}
}
