package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

// cliIndexServer runs a real codeindex handler (no full skopos serve) for
// CLI protocol tests.
func cliIndexServer(t *testing.T, apiKey string) *httptest.Server {
	t.Helper()
	store, err := codeindex.NewStore(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	h := codeindex.NewHandler(codeindex.NewService(store), apiKey)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/codeindex/{workspace}/manifest", h.Manifest)
	mux.HandleFunc("POST /api/codeindex/{workspace}/blobs", h.Blobs)
	mux.HandleFunc("POST /api/codeindex/{workspace}/commit", h.Commit)
	mux.HandleFunc("GET /api/codeindex/{workspace}/status", h.Status)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func cliRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte(`package main

func Alpha() string { return beta() }

func beta() string { return "b" }
`), 0o644)
	return root
}

func TestPushToServerFullFlow(t *testing.T) {
	ts := cliIndexServer(t, "")
	repo := cliRepo(t)
	ctx := context.Background()

	uploaded, total, err := PushToServer(ctx, ts.URL, "", "github.com/test/cli", "main", repo)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || uploaded != 1 {
		t.Fatalf("first push: uploaded=%d total=%d", uploaded, total)
	}

	// Second push: nothing new to upload (manifest dedup).
	uploaded, total, err = PushToServer(ctx, ts.URL, "", "github.com/test/cli", "main", repo)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || uploaded != 0 {
		t.Fatalf("incremental push: uploaded=%d total=%d", uploaded, total)
	}

	// The server answers queries for the pushed branch.
	status, err := getJSON[[]codeindex.BranchStatus](ctx, ts.URL, "", "/api/codeindex/github.com%2Ftest%2Fcli/status")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].Branch != "main" || status[0].SymbolCount != 2 {
		t.Fatalf("status: %+v", status)
	}
}

func TestPushToServerErrorSurfacesServerMessage(t *testing.T) {
	// Server that rejects the manifest with a JSON error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "branch is required"})
	}))
	defer ts.Close()

	_, _, err := PushToServer(context.Background(), ts.URL, "", "ws", "main", cliRepo(t))
	if err == nil {
		t.Fatal("expected error")
	}
	// apiErrorMessage must surface the server's message, not just the status.
	if want := "branch is required"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error should include server message %q, got: %v", want, err)
	}
}

func TestBuildRelativizesPaths(t *testing.T) {
	repo := cliRepo(t)
	results, head, err := codeindex.Build(context.Background(), parse.NewExtractor(), repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "main.go" {
		t.Fatalf("paths not relative: %+v", results)
	}
	if head != "" && len(head) != 40 {
		t.Fatalf("unexpected head sha %q", head)
	}
	// No git repo in a temp dir: head is empty.
	if head != "" {
		t.Fatalf("expected empty head outside a git repo, got %q", head)
	}
}

func TestPushToServerUnreachableHint(t *testing.T) {
	// Nothing listens on this port.
	_, _, err := PushToServer(context.Background(), "http://127.0.0.1:1", "", "ws", "main", cliRepo(t))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "skopos index build") {
		t.Fatalf("error should hint at the local-only alternative: %v", msg)
	}
}

func TestPushLargeRepoExceedingLegacyLimits(t *testing.T) {
	ts := cliIndexServer(t, "")
	repo := t.TempDir()
	// ~2200 files with long paths: commit JSON alone is ~2MB (over the old
	// 1 MiB cap) and blobs force several upload chunks (512 blobs/chunk).
	for i := 0; i < 2200; i++ {
		dir := filepath.Join(repo, fmt.Sprintf("pkg%03d/module%03d/internal%03d/deep/nested/path/segments%03d", i%50, i%97, i, i%13))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%04d.go", i)),
			[]byte(fmt.Sprintf("package p%d\n\nfunc Fn%04d() {}\n", i, i)), 0o644)
	}
	uploaded, total, err := PushToServer(context.Background(), ts.URL, "", "big/repo", "main", repo)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if total != 2200 || uploaded != 2200 {
		t.Fatalf("uploaded=%d total=%d", uploaded, total)
	}
	// Commit landed: status reports the file count.
	status, err := getJSON[[]codeindex.BranchStatus](context.Background(), ts.URL, "", "/api/codeindex/big%2Frepo/status")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].FileCount != 2200 {
		t.Fatalf("status after big push: %+v", status)
	}
	// Incremental push uploads nothing.
	uploaded, _, err = PushToServer(context.Background(), ts.URL, "", "big/repo", "main", repo)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded != 0 {
		t.Fatalf("incremental uploaded=%d, want 0", uploaded)
	}
}
