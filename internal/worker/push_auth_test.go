package worker

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// GitHub pushes authenticate with HTTP Basic (user x-access-token, the
// token as the password); a Bearer header is rejected by GitHub's git over
// HTTPS ("remote: invalid credentials"). Gitea keeps its "token" scheme.
func TestPushAuthHeader(t *testing.T) {
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:github_pat_abc"))
	if got := pushAuthHeader("github", "github_pat_abc"); got != want {
		t.Fatalf("github header = %q, want %q", got, want)
	}
	if got := pushAuthHeader("gitea", "t0k"); got != "Authorization: token t0k" {
		t.Fatalf("gitea header = %q", got)
	}
}

// What git actually sends: push to a local HTTP server that records the
// Authorization header of the receive-pack discovery request (and refuses
// it — only the header matters here).
func TestPushBranchSendsGitHubBasicAuth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"checkout", "-q", "-b", "agent/x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	err := pushBranch(context.Background(), dir, srv.URL+"/martinsuchenak/skopos-sandbox.git", "github_pat_abc", "github", "agent/x", "agent/", []string{"main"})
	if err == nil {
		t.Fatal("the test server refuses every push")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:github_pat_abc"))
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatalf("git never reached the server: %v", err)
	}
	for _, h := range seen {
		if h != want {
			t.Fatalf("git sent Authorization %q, want %q (all: %s)", h, want, strings.Join(seen, " | "))
		}
	}
}
