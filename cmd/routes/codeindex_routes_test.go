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
	"path/filepath"
	"strings"
	"testing"

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
