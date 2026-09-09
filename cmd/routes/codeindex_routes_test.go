package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

func codeindexSetup(t *testing.T, apiKey string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(filepath.Join(repo, "pkg"), 0o755)
	os.WriteFile(filepath.Join(repo, "main.go"), []byte(`package main

func Handler() string { return helper() }

func helper() string { return "x" }
`), 0o644)

	store, err := codeindex.NewStore(filepath.Join(dir, "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	h := codeindex.NewHandler(codeindex.NewService(store), apiKey)

	mux := http.NewServeMux()
	registerCodeIndexRoutes(mux, h)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, repo
}

// pushRepo drives the same client flow as `skopos index push`:
// manifest -> upload missing blobs -> commit.
func pushRepo(t *testing.T, ts *httptest.Server, key, repo, workspace, branch string) {
	t.Helper()
	ctx := context.Background()
	results, head, err := codeindex.Build(ctx, parse.NewExtractor(), repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	type fileEntry struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
	}
	entries := make([]fileEntry, 0, len(results))
	byHash := map[string]*parse.FileResult{}
	for _, r := range results {
		entries = append(entries, fileEntry{Path: r.Path, Hash: r.Hash})
		byHash[r.Hash] = r
	}

	// manifest
	body, _ := json.Marshal(map[string]any{"files": entries})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+fmt.Sprintf("/api/codeindex/%s/manifest", url.PathEscape(workspace)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var man struct {
		Missing []string `json:"missing"`
	}
	json.NewDecoder(resp.Body).Decode(&man)
	resp.Body.Close()
	if len(man.Missing) != len(entries) {
		t.Fatalf("manifest: expected %d missing, got %v", len(entries), man.Missing)
	}

	// blobs (ndjson)
	var nd bytes.Buffer
	for _, h := range man.Missing {
		json.NewEncoder(&nd).Encode(byHash[h])
	}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+fmt.Sprintf("/api/codeindex/%s/blobs", url.PathEscape(workspace)), bytes.NewReader(nd.Bytes()))
	req.Header.Set("Content-Type", "application/x-ndjson")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]int
	json.NewDecoder(resp.Body).Decode(&stored)
	resp.Body.Close()
	if stored["stored"] != len(entries) {
		t.Fatalf("blobs: %+v", stored)
	}

	// commit
	body, _ = json.Marshal(map[string]any{"branch": branch, "head_sha": head, "source": "test", "files": entries})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+fmt.Sprintf("/api/codeindex/%s/commit", url.PathEscape(workspace)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit: %d", resp.StatusCode)
	}
}

func TestCodeIndexPushSearchGraph(t *testing.T) {
	ts, repo := codeindexSetup(t, "")
	pushRepo(t, ts, "", repo, "github.com/example/repo", "main")

	// search by split camel/snake token
	resp, _ := http.Get(ts.URL + "/api/codeindex/github.com%2Fexample%2Frepo/search?q=handler")
	var res codeindex.SearchResults
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if len(res.Hits) != 1 || res.Hits[0].Name != "Handler" {
		t.Fatalf("search: %+v", res.Hits)
	}

	// callers of helper -> Handler
	resp, _ = http.Get(ts.URL + "/api/codeindex/github.com%2Fexample%2Frepo/callers?name=helper")
	var callers codeindex.GraphResults
	json.NewDecoder(resp.Body).Decode(&callers)
	resp.Body.Close()
	if len(callers.Edges) != 1 || callers.Edges[0].Caller != "Handler" {
		t.Fatalf("callers: %+v", callers.Edges)
	}

	// status
	resp, _ = http.Get(ts.URL + "/api/codeindex/github.com%2Fexample%2Frepo/status")
	var status []codeindex.BranchStatus
	json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if len(status) != 1 || status[0].Branch != "main" || status[0].SymbolCount != 2 {
		t.Fatalf("status: %+v", status)
	}

	// unknown branch falls back with a note
	resp, _ = http.Get(ts.URL + "/api/codeindex/github.com%2Fexample%2Frepo/search?q=handler&branch=feat%2Fx")
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if !res.Fallback || !strings.Contains(res.Note, "feat/x") {
		t.Fatalf("fallback: %+v", res)
	}
}

func TestCodeIndexRequiresAuth(t *testing.T) {
	ts, _ := codeindexSetup(t, "secret")
	resp, err := http.Get(ts.URL + "/api/codeindex/ws/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/codeindex/ws/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keyed status: %d", resp.StatusCode)
	}
}

func TestCodeIndexBadContentType(t *testing.T) {
	ts, _ := codeindexSetup(t, "")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/codeindex/ws/blobs", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json") // must be ndjson
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", resp.StatusCode)
	}
}

func TestCodeIndexServerSideRefresh(t *testing.T) {
	ts, repo := codeindexSetup(t, "")

	// Turn the fixture repo into a git repo and register its git_url.
	run := func(args ...string) string {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", args, out)
		}
		return string(out)
	}
	run("git", "init", "-q", "-b", "main", ".")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "t")
	run("git", "add", ".")
	run("git", "commit", "-qm", "init")

	// Register the workspace with a git_url.
	body, _ := json.Marshal(map[string]any{"id": "github.com/example/repo", "git_url": repo})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/workspaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The routes mux has no /api/workspaces; register git_url directly through
	// a dedicated refresh setup instead: re-run with the refresher wired.
	// (Simpler: this test constructs its own mux with a refresher.)
	_ = req

	// Build the refresher-backed mux.
	store2, err := codeindex.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store2.Close)
	h2 := codeindex.NewHandler(codeindex.NewService(store2), "")
	ref, err := codeindex.NewRefresher(store2, t.TempDir(), func(id string) (string, error) {
		if id == "github.com/example/repo" {
			return repo, nil
		}
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h2.SetRefresher(ref)
	mux := http.NewServeMux()
	registerCodeIndexRoutes(mux, h2)
	ts2 := httptest.NewServer(mux)
	t.Cleanup(ts2.Close)

	// Start the refresh and wait for completion.
	req, _ = http.NewRequest(http.MethodPost, ts2.URL+"/api/codeindex/github.com%2Fexample%2Frepo/refresh", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("refresh start: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := getRefreshState(ts2.URL + "/api/codeindex/github.com%2Fexample%2Frepo/refresh")
		if err != nil {
			t.Fatal(err)
		}
		if !state.Building {
			if state.LastError != "" {
				t.Fatalf("refresh failed: %s", state.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish in time")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The server-built branch is queryable.
	resp, err = http.Get(ts2.URL + "/api/codeindex/github.com%2Fexample%2Frepo/search?q=handler")
	if err != nil {
		t.Fatal(err)
	}
	var res codeindex.SearchResults
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if len(res.Hits) == 0 {
		t.Fatalf("server-built index has no hits: %+v", res)
	}

	// Duplicate refresh while idle is allowed; state endpoint reflects idle.
	if s := getRefreshStateTS(t, ts2.URL); s.Building {
		t.Fatal("state should be idle after completion")
	}
}

func getRefreshStateTS(t *testing.T, base string) codeindex.RefreshState {
	t.Helper()
	s, err := getRefreshState(base + "/api/codeindex/github.com%2Fexample%2Frepo/refresh")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func getRefreshState(url string) (codeindex.RefreshState, error) {
	resp, err := http.Get(url)
	if err != nil {
		return codeindex.RefreshState{}, err
	}
	defer resp.Body.Close()
	var s codeindex.RefreshState
	json.NewDecoder(resp.Body).Decode(&s)
	return s, nil
}
