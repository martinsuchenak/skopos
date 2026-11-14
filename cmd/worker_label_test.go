package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/paularlott/cli"
)

// skopos worker label sends the whole list (normalised) with the caller's
// key from the environment — never the worker's env file, whose key may not
// label a worker.
func TestWorkerLabelCommand(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Skopos-Via")+" "+string(body))
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id":"worker-box","labels":["stock"]}]`))
			return
		}
		_, _ = w.Write([]byte(`{"labels":["plant-2","stock"]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SKOPOS_API_KEY", "sk_approver")
	t.Setenv("SKOPOS_SERVER_URL", srv.URL)
	t.Chdir(t.TempDir())
	run := func(args ...string) error {
		t.Helper()
		old := os.Args
		defer func() { os.Args = old }()
		os.Args = append([]string{"skopos", "worker", "label"}, args...)
		root := &cli.Command{Name: "skopos", Commands: []*cli.Command{{Name: "worker", Commands: []*cli.Command{workerLabelCmd()}}}}
		return root.Execute(context.Background())
	}
	if err := run("--id", "worker-box", "--set", "Stock, plant-2,stock"); err != nil {
		t.Fatal(err)
	}
	if err := run("--id", "worker-box", "--clear"); err != nil {
		t.Fatal(err)
	}
	if err := run("--id", "worker-box"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PUT /api/workers/worker-box/labels Bearer sk_approver cli {"labels":["plant-2","stock"]}`,
		`PUT /api/workers/worker-box/labels Bearer sk_approver cli {"labels":[]}`,
		`GET /api/workers Bearer sk_approver  `,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s", strings.Join(got, "\n"))
	}
	if err := run("--id", "worker-box", "--set", "has space"); err == nil || len(got) != 3 {
		t.Fatalf("an invalid label is refused before any request: %v", err)
	}
	if err := run("--set", "stock"); err == nil {
		t.Fatal("--id is required")
	}
	if err := run("--id", "worker-none"); err == nil || !strings.Contains(err.Error(), "no worker worker-none") {
		t.Fatalf("an unknown worker: %v", err)
	}
}
