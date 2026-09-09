package codeindex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

func analysisRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "app.go"), []byte(`package main

func main() {
	c := NewCache()
	c.Put("k", 1)
	used(c)
}

func NewCache() *Cache { return &Cache{} }

type Cache struct{}

func (c *Cache) Put(k string, v int) {}

func used(c *Cache) {}

func orphan() {}

func chainA() { chainB() }
func chainB() { chainA() }
`), 0o644)
	return root
}

func TestAnalysisDeadCyclesTree(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, analysisRepo(t), "main")
	svc := NewService(store)
	ctx := context.Background()

	// Dead: orphan has no callers; main/NewCache excluded or called.
	dead, err := svc.Dead(ctx, "ws", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range dead.Symbols {
		names[s.Name] = true
	}
	if !names["orphan"] {
		t.Fatalf("expected orphan in dead list, got %v", names)
	}
	if names["main"] || names["NewCache"] {
		t.Fatalf("entry points must be excluded, got %v", names)
	}
	_ = parse.NewExtractor // keep import if assertions change

	// Cycles: chainA <-> chainB detected.
	cycles, err := svc.Cycles(ctx, "ws", "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cycles.Cycles {
		joined := c.Names[0] + "|" + c.Names[1]
		if joined == "chainA|chainB" || joined == "chainB|chainA" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected chainA<->chainB cycle, got %+v", cycles.Cycles)
	}

	// Call tree from main contains NewCache and used.
	tree, err := svc.CallTree(ctx, "ws", "main", "main", 2)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(n CallTreeNode) map[string]bool
	walk = func(n CallTreeNode) map[string]bool {
		out := map[string]bool{n.Name: true}
		for _, c := range n.Children {
			for k := range walk(c) {
				out[k] = true
			}
		}
		return out
	}
	reached := walk(tree.Tree)
	if !reached["NewCache"] || !reached["used"] {
		t.Fatalf("call tree from main: %v", reached)
	}
}

func TestBranchDiff(t *testing.T) {
	root := analysisRepo(t)
	store := newTestStore(t)
	buildInto(t, store, root, "main")

	// Feature branch: add a file, change app.go, delete nothing.
	os.WriteFile(filepath.Join(root, "extra.go"), []byte("package main\n\nfunc Extra() {}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "app.go"), []byte(`package main

func main() {
	renamedHelper()
}

func renamedHelper() {}
`), 0o644)
	buildInto(t, store, root, "feat/x")
	svc := NewService(store)

	diff, err := svc.BranchDiff(context.Background(), "ws", "feat/x")
	if err != nil {
		t.Fatal(err)
	}
	fileChanges := map[string]string{}
	for _, f := range diff.Files {
		fileChanges[f.Path] = f.Change
	}
	if fileChanges["extra.go"] != "added" || fileChanges["app.go"] != "changed" {
		t.Fatalf("file changes: %v", fileChanges)
	}
	symChanges := map[string]string{}
	for _, s := range diff.Symbols {
		symChanges[s.Name] = s.Change
	}
	if symChanges["Extra"] != "added" || symChanges["renamedHelper"] != "added" {
		t.Fatalf("symbol deltas missing additions: %v", symChanges)
	}
	if symChanges["orphan"] != "removed" {
		t.Fatalf("expected removed symbols from changed app.go: %v", symChanges)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	root := analysisRepo(t)
	store := newTestStore(t)
	buildInto(t, store, root, "main")
	svc := NewService(store)

	var buf bytes.Buffer
	if err := svc.ExportNDJSON("ws", "main", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty export")
	}

	// Import into a fresh workspace.
	store2, err := NewStore(filepath.Join(t.TempDir(), "idx2"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store2.Close)
	svc2 := NewService(store2)
	branches, err := svc2.ImportNDJSON("ws2", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0] != "main" {
		t.Fatalf("branches: %v", branches)
	}

	// The imported index answers the same query.
	res, err := svc2.Search(context.Background(), "ws2", "main", "orphan", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Name != "orphan" {
		t.Fatalf("imported search: %+v", res.Hits)
	}
}

func TestQualifiedNamesDisambiguateGraph(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.php"), []byte(`<?php
class Alpha {
  public function validate(): bool { return $this->check(); }
  public function check(): bool { return true; }
}
`), 0o644)
	os.WriteFile(filepath.Join(root, "b.php"), []byte(`<?php
class Beta {
  public function validate(): bool { return true; }
  public function run(): void { $this->validate(); }
}
`), 0o644)
	store := newTestStore(t)
	buildInto(t, store, root, "main")
	svc := NewService(store)
	ctx := context.Background()

	// who-calls validate: both classes' call sites, qualified per class.
	res, err := svc.Callers(ctx, "ws", "main", "validate", 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range res.Edges {
		seen[e.Caller+"/"+e.Callee] = true
	}
	if !seen["Beta::run/Beta::validate"] {
		t.Fatalf("Beta::run -> Beta::validate missing: %+v", res.Edges)
	}
	// No caller of Alpha::validate exists (it is only called via $this? no —
	// nothing calls Alpha::validate), so it must NOT appear as an impact root
	// with callers, while Beta::validate has run.
	impact, err := svc.Impact(ctx, "ws", "main", "validate", 3)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range impact.Affected {
		names[a.Name] = true
		if a.Path == "" {
			t.Fatalf("impact node without definition location: %+v", a)
		}
	}
	if !names["Beta::run"] {
		t.Fatalf("impact missed Beta::run: %+v", impact.Affected)
	}

	// Fully-qualified lookup is precise: Alpha::validate only.
	impactA, err := svc.Impact(ctx, "ws", "main", "Alpha::validate", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(impactA.Affected) != 0 {
		t.Fatalf("Alpha::validate has no callers: %+v", impactA.Affected)
	}

	// Symbol lookup accepts the qualified name.
	sym, err := svc.Symbol(ctx, "ws", "main", "Alpha::validate")
	if err != nil {
		t.Fatal(err)
	}
	if len(sym.Hits) != 1 || sym.Hits[0].Qualified != "Alpha::validate" {
		t.Fatalf("qualified symbol lookup: %+v", sym.Hits)
	}
}

func TestCallTreeResolvesUniqueBareNames(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "svc.php"), []byte(`<?php
class Svc {
  public function outer(): void { $this->helper(); }
  public function helper(): void { $this->inner(); }
  public function inner(): void { leafWork(); }
}
function leafWork() {}
`), 0o644)
	os.WriteFile(filepath.Join(root, "other.php"), []byte(`<?php
class Other {
  public function helper(): void {} // same short name: helper is ambiguous
}
`), 0o644)
	store := newTestStore(t)
	buildInto(t, store, root, "main")
	svc := NewService(store)

	// helper is ambiguous (Svc::helper + Other::helper) -> stays bare in the tree.
	res, err := svc.CallTree(context.Background(), "ws", "main", "Svc::outer", 3)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var walk func(n CallTreeNode)
	walk = func(n CallTreeNode) {
		names = append(names, n.Name)
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(res.Tree)
	joined := strings.Join(names, ",")
	if strings.Contains(joined, "helper::") {
		t.Fatalf("ambiguous helper must stay bare: %v", names)
	}
	// leafWork is a unique top-level function: stays bare (correctly).
	if !strings.Contains(joined, "leafWork") {
		t.Fatalf("leafWork missing: %v", names)
	}
}
