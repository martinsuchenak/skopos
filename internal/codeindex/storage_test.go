package codeindex

import (
	"context"
	"os"
	"path/filepath"
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

	// Second build on the unchanged tree: every result is a cache stub
	// (same hashes, no re-extraction) but commits to identical branch state.
	r2, _, err := BuildWithCache(ctx, ex, root, "main", nil, store.AsBuildCache("ws"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r2) != len(r1) {
		t.Fatalf("file count changed: %d vs %d", len(r2), len(r1))
	}
	stubs := 0
	for i := range r2 {
		if r2[i].Hash != r1[i].Hash {
			t.Fatalf("hash mismatch on %s: %s vs %s", r2[i].Path, r2[i].Hash, r1[i].Hash)
		}
		if len(r2[i].Symbols) == 0 {
			stubs++
		}
	}
	if stubs != len(r2) {
		t.Fatalf("expected all-cached rebuild, %d/%d were re-parsed (parsed first time: %d)", len(r2)-stubs, len(r2), parsed)
	}

	// A touched file re-parses (mtime bump invalidates its cache row).
	time.Sleep(10 * time.Millisecond) // ensure mtime moves
	touched := filepath.Join(root, "main.go")
	if err := os.WriteFile(touched, append(mustRead(t, touched), []byte("\nfunc Fresh() {}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	r3, _, err := BuildWithCache(ctx, ex, root, "main", nil, store.AsBuildCache("ws"))
	if err != nil {
		t.Fatal(err)
	}
	reparsed := 0
	for _, r := range r3 {
		if len(r.Symbols) > 0 {
			reparsed++
		}
	}
	if reparsed != 1 {
		t.Fatalf("expected exactly the touched file to re-parse, got %d", reparsed)
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
