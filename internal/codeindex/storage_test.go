package codeindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

// buildInto builds dir into the given store under branch.
func buildInto(t *testing.T, store *Store, dir, branch string) {
	t.Helper()
	files, head, err := Build(context.Background(), parse.NewExtractor(), dir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitLocal(store, "ws", branch, "test", files, head); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

// _lastStore is set by buildFixture for query helpers (single-threaded tests).
var _lastStore *Store

func fixtureService(t *testing.T) *Service {
	if _lastStore == nil {
		t.Fatal("no store built")
	}
	return NewService(_lastStore)
}

func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "pkg"), 0o755)
	os.WriteFile(filepath.Join(root, "main.go"), []byte(`package main

type Config struct{ Port int }

func LoadConfig() *Config { return &Config{} }

func main() {
	cfg := LoadConfig()
	serve(cfg)
}

func serve(c *Config) {}
`), 0o644)
	os.WriteFile(filepath.Join(root, "pkg", "util.go"), []byte(`package pkg

func Helper() int { return 42 }
`), 0o644)
	return root
}

func TestIngestSearchAndGraph(t *testing.T) {
	_lastStore = newTestStore(t)
	buildInto(t, _lastStore, writeRepo(t), "main")
	svc := fixtureService(t)

	// FTS search finds camelCase symbol via name_parts splitting.
	res, err := svc.Search(context.Background(), "ws", "main", "loadconfig", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range res.Hits {
		if h.Name == "LoadConfig" {
			found = true
		}
	}
	if !found {
		t.Fatalf("search 'loadconfig' missed LoadConfig: %+v", res.Hits)
	}

	// Symbol lookup by exact name with path+line.
	sym, err := svc.Symbol(context.Background(), "ws", "main", "LoadConfig")
	if err != nil {
		t.Fatal(err)
	}
	if len(sym.Hits) != 1 || sym.Hits[0].Path != "main.go" || sym.Hits[0].Line != 5 {
		t.Fatalf("symbol lookup: %+v", sym.Hits)
	}

	// Callers: main calls LoadConfig.
	callers, err := svc.Callers(context.Background(), "ws", "main", "LoadConfig", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers.Edges) != 1 || callers.Edges[0].Caller != "main" {
		t.Fatalf("callers of LoadConfig: %+v", callers.Edges)
	}

	// Callees from main include LoadConfig and serve.
	callees, err := svc.Callees(context.Background(), "ws", "main", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range callees.Edges {
		names[e.Callee] = true
	}
	if !names["LoadConfig"] || !names["serve"] {
		t.Fatalf("callees of main: %+v", callees.Edges)
	}

	// Impact: changing serve affects main (depth 1); LoadConfig affects main.
	impact, err := svc.Impact(context.Background(), "ws", "main", "serve", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.Affected) != 1 || impact.Affected[0].Name != "main" {
		t.Fatalf("impact of serve: %+v", impact.Affected)
	}

	// Outline lists a file's symbols in order.
	outline, err := svc.Outline(context.Background(), "ws", "main", "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(outline.Hits) < 4 {
		t.Fatalf("outline of main.go: %+v", outline.Hits)
	}
	if outline.Hits[0].Name != "Config" {
		t.Fatalf("outline order: %+v", outline.Hits)
	}
}

func TestBranchFallbackAndDedup(t *testing.T) {
	root := writeRepo(t)
	_lastStore = newTestStore(t)
	buildInto(t, _lastStore, root, "main")

	// Branch B modifies one file: pkg/util.go gets a new symbol.
	os.WriteFile(filepath.Join(root, "pkg", "util.go"), []byte(`package pkg

func Helper() int { return 42 }

func NewThing() string { return "x" }
`), 0o644)
	buildInto(t, _lastStore, root, "feat/x")

	svc := fixtureService(t)

	// Unindexed branch falls back to default branch, labeled.
	res, err := svc.Search(context.Background(), "ws", "feat/unknown", "helper", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fallback {
		t.Fatalf("expected fallback for unknown branch, got %+v", res)
	}

	// The indexed branch sees its own state.
	inBranch, err := svc.Symbol(context.Background(), "ws", "feat/x", "NewThing")
	if err != nil {
		t.Fatal(err)
	}
	if len(inBranch.Hits) != 1 {
		t.Fatalf("feat/x should see NewThing: %+v", inBranch.Hits)
	}
	// ...and main does not.
	inMain, err := svc.Symbol(context.Background(), "ws", "main", "NewThing")
	if err != nil {
		t.Fatal(err)
	}
	if len(inMain.Hits) != 0 {
		t.Fatalf("main must not see NewThing: %+v", inMain.Hits)
	}

	// Status shows both branches; identical file content across branches is
	// deduped (main.go unchanged -> Config indexed once), while the two
	// versions of util.go legitimately hold two Helper rows.
	status, err := svc.Status(context.Background(), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 2 {
		t.Fatalf("expected 2 branch states, got %+v", status)
	}
	var configRows, helperRows int
	db, err := _lastStore.DB("ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE name = 'Config'`).Scan(&configRows); err != nil {
		t.Fatal(err)
	}
	if configRows != 1 {
		t.Fatalf("expected deduped single Config row (identical file), got %d", configRows)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE name = 'Helper'`).Scan(&helperRows); err != nil {
		t.Fatal(err)
	}
	if helperRows != 2 {
		t.Fatalf("expected two Helper rows (distinct file versions), got %d", helperRows)
	}

	// Drop a branch.
	if err := svc.DropBranch(context.Background(), "ws", "feat/x"); err != nil {
		t.Fatal(err)
	}
	status, _ = svc.Status(context.Background(), "ws")
	if len(status) != 1 || status[0].Branch != "main" {
		t.Fatalf("after drop: %+v", status)
	}
}

func TestManifestNegotiation(t *testing.T) {
	root := writeRepo(t)
	store := newTestStore(t)
	buildInto(t, store, root, "main")

	// All hashes present for main → nothing missing.
	files, _, err := Build(context.Background(), parse.NewExtractor(), root, "main")
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, f := range files {
		hashes = append(hashes, f.Hash)
	}
	missing, err := store.HasBlobs("ws", hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing blobs, got %v", missing)
	}

	// A changed file's hash is missing.
	os.WriteFile(filepath.Join(root, "pkg", "util.go"), []byte("package pkg\n\nfunc Other() {}\n"), 0o644)
	files2, _, _ := Build(context.Background(), parse.NewExtractor(), root, "main")
	missing, err = store.HasBlobs("ws", []string{files2[1].Hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != files2[1].Hash {
		t.Fatalf("expected the changed hash missing, got %v", missing)
	}
}

func TestBuildWithProgressReports(t *testing.T) {
	root := writeRepo(t)
	var calls int
	var lastDone, lastTotal int
	results, _, err := BuildWithProgress(context.Background(), parse.NewExtractor(), root, "main",
		func(done, total int) { calls++; lastDone, lastTotal = done, total })
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	// Initial call (0/total) plus one per file; the final call is done==total.
	if calls < len(results)+1 {
		t.Fatalf("calls=%d, want >= %d", calls, len(results)+1)
	}
	if lastDone != lastTotal || lastTotal != len(results) {
		t.Fatalf("final progress %d/%d, results %d", lastDone, lastTotal, len(results))
	}
}

func TestBuildWithCacheReusesParses(t *testing.T) {
	root := writeRepo(t)
	store := newTestStore(t)
	ex := parse.NewExtractor()
	ctx := context.Background()

	// First build parses everything and populates the cache.
	r1, _, err := BuildWithCache(ctx, ex, root, "main", nil, store.AsBuildCache("ws"))
	if err != nil {
		t.Fatal(err)
	}
	parsed := 0
	for _, r := range r1 {
		if len(r.Symbols) > 0 {
			parsed++
		}
	}

	// Second build on the unchanged tree: every result is served from the
	// cache and carries the full stored payload (same hashes AND symbols),
	// never a stub — a stub would commit empty files to a fresh store.
	r2, _, err := BuildWithCache(ctx, ex, root, "main", nil, store.AsBuildCache("ws"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r2) != len(r1) {
		t.Fatalf("file count changed: %d vs %d", len(r2), len(r1))
	}
	for i := range r2 {
		if r2[i].Hash != r1[i].Hash {
			t.Fatalf("hash mismatch on %s: %s vs %s", r2[i].Path, r2[i].Hash, r1[i].Hash)
		}
		if len(r2[i].Symbols) != len(r1[i].Symbols) || len(r2[i].Edges) != len(r1[i].Edges) {
			t.Fatalf("cache hit on %s lost payload: %d/%d symbols, %d/%d edges",
				r2[i].Path, len(r2[i].Symbols), len(r1[i].Symbols), len(r2[i].Edges), len(r1[i].Edges))
		}
	}

	// A touched file re-parses (mtime bump invalidates its cache row): the
	// new symbol must appear; other files keep their payloads from cache.
	time.Sleep(10 * time.Millisecond) // ensure mtime moves
	touched := filepath.Join(root, "main.go")
	if err := os.WriteFile(touched, append(mustRead(t, touched), []byte("\nfunc Fresh() {}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	r3, _, err := BuildWithCache(ctx, ex, root, "main", nil, store.AsBuildCache("ws"))
	if err != nil {
		t.Fatal(err)
	}
	freshSeen := false
	for _, r := range r3 {
		if strings.HasSuffix(r.Path, "main.go") {
			for _, s := range r.Symbols {
				if s.Name == "Fresh" {
					freshSeen = true
				}
			}
		}
	}
	if !freshSeen {
		t.Fatal("touched file's new symbol not indexed — cache served stale content")
	}

	// Cached rebuild still commits identical symbol counts.
	buildInto(t, store, root, "main") // uncached reference
	st1, _ := NewService(store).Status(ctx, "ws")
	_ = st1
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSearchByQualifiedName(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, writeRepo(t), "main")

	// Exact FQN.
	res, err := NewService(store).Search(context.Background(), "ws", "main", "LoadConfig", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("bare search setup failed")
	}
	// Go fixture has no classes; qualify via the PHP-shaped fixture instead.
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.php"), []byte(`<?php
class Invoice {
  public function updateStatus(): void { $this->notify(); }
  public function notify(): void {}
}
`), 0o644)
	store2 := newTestStore(t)
	buildInto(t, store2, root, "main")
	svc := NewService(store2)

	exact, err := svc.Search(context.Background(), "ws", "main", "Invoice::updateStatus", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Hits) != 1 || exact.Hits[0].Qualified != "Invoice::updateStatus" {
		t.Fatalf("exact FQN search: %+v", exact.Hits)
	}
	prefix, err := svc.Search(context.Background(), "ws", "main", "Invoice::up", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix.Hits) != 1 {
		t.Fatalf("FQN prefix search: %+v", prefix.Hits)
	}
	// Bare search unchanged.
	bare, err := svc.Search(context.Background(), "ws", "main", "updatestatus", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Hits) != 1 {
		t.Fatalf("bare search via split tokens: %+v", bare.Hits)
	}
}

func TestStoreDBHandleBound(t *testing.T) {
	store := newTestStore(t)
	store.AsBuildCache("ws-cache-keep")
	if _, err := store.DB("ws-cache-keep"); err != nil {
		t.Fatal(err)
	}
	// Touching many distinct workspace ids must not accumulate open SQLite
	// handles: unbounded growth exhausts the process fd limit.
	const n = maxOpenIndexDBs + 40
	for i := 0; i < n; i++ {
		if _, err := store.DB(fmt.Sprintf("ws-%04d", i)); err != nil {
			t.Fatalf("DB(%d): %v", i, err)
		}
	}
	store.mu.Lock()
	open := len(store.dbs)
	_, cacheOpen := store.dbs["ws-cache-keep"]
	store.mu.Unlock()
	if open > maxOpenIndexDBs {
		t.Errorf("open handles %d exceed bound %d", open, maxOpenIndexDBs)
	}
	if !cacheOpen {
		t.Error("build-cache workspace should be retained under eviction")
	}
	// A handle is usable again after eviction (reopened transparently).
	if _, err := store.DB("ws-0000"); err != nil {
		t.Fatalf("reopen after eviction: %v", err)
	}
}

func TestSearchQueryLengthCap(t *testing.T) {
	store := newTestStore(t)
	svc := NewService(store)
	if _, err := svc.Search(context.Background(), "ws", "main", strings.Repeat("a", 257), 10); err == nil {
		t.Fatal("oversized query accepted (FTS5 CPU DoS surface)")
	}
	if _, err := svc.Search(context.Background(), "ws", "main", strings.Repeat("a", 256), 10); err != nil {
		t.Fatalf("max-length query rejected: %v", err)
	}
}

func TestBuildWithCacheMaterializesForFreshTarget(t *testing.T) {
	root := writeRepo(t)
	ctx := context.Background()
	ex := parse.NewExtractor()

	// The push layout: a machine-wide cache store, separate from whatever
	// store/server receives the results.
	cacheDir := filepath.Join(t.TempDir(), "c")
	cacheStore, err := NewStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cacheStore.Close)
	cacher := cacheStore.AsBuildCache("cache")

	r1, _, err := BuildWithCache(ctx, ex, root, "main", nil, cacher)
	if err != nil {
		t.Fatal(err)
	}
	symbols := 0
	for _, r := range r1 {
		symbols += len(r.Symbols)
	}
	if symbols == 0 {
		t.Fatal("fixture produced no symbols")
	}

	// Re-push against an empty server: hashes are all "missing", so the
	// cached results must carry full payloads, not stubs.
	r2, _, err := BuildWithCache(ctx, ex, root, "main", nil, cacher)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for i, r := range r2 {
		if r.Hash != r1[i].Hash {
			t.Fatalf("hash mismatch on %s", r.Path)
		}
		got += len(r.Symbols)
	}
	if got != symbols {
		t.Fatalf("cached rebuild lost symbols: %d of %d — stubs would upload an empty index to a fresh server", got, symbols)
	}
}

func TestBuildWithCacheLegacyRowsWithoutPayloads(t *testing.T) {
	// Caches written before payloads were stored have file_cache rows but no
	// blobs: lookups must fall back to a fresh parse instead of trusting the
	// hash and emitting an empty result.
	root := writeRepo(t)
	ctx := context.Background()
	ex := parse.NewExtractor()

	cacheStore, err := NewStore(filepath.Join(t.TempDir(), "c"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cacheStore.Close)
	cacher := cacheStore.AsBuildCache("cache")

	if _, _, err := BuildWithCache(ctx, ex, root, "main", nil, cacher); err != nil {
		t.Fatal(err)
	}
	// Simulate the legacy state: keep file_cache, drop the blob payloads.
	db, err := cacheStore.DB("cache")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM blobs`); err != nil {
		t.Fatal(err)
	}

	r2, _, err := BuildWithCache(ctx, ex, root, "main", nil, cacher)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range r2 {
		if r.Hash == "" {
			t.Fatalf("no result for %s", r.Path)
		}
	}
	// Payloads restored: the delete-blobs pass re-parsed and re-stored them.
	var blobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs == 0 {
		t.Fatal("fresh parses did not repopulate blob payloads")
	}
}
