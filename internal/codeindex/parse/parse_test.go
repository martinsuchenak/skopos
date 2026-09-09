package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseGoSymbolsAndCalls(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "sample.go", `package main

type Server struct{ Port int }

func (s *Server) Start() error {
	db := openDB()
	return db.Ping()
}

func openDB() (*Conn, error) { return nil, nil }
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Lang != "go" {
		t.Fatalf("lang: %q", res.Lang)
	}
	if res.Err {
		t.Fatal("unexpected error nodes")
	}
	want := map[string]string{
		"Server": "struct", "Start": "method", "openDB": "func",
	}
	for _, s := range res.Symbols {
		if want[s.Name] == s.Kind {
			delete(want, s.Name)
		}
	}
	if len(want) > 0 {
		t.Fatalf("missing symbols %v; got %+v", want, res.Symbols)
	}
	callees := map[string]bool{}
	for _, e := range res.Edges {
		callees[e.Callee] = true
	}
	if !callees["openDB"] || !callees["Ping"] {
		t.Fatalf("expected call edges to openDB and Ping, got %+v", res.Edges)
	}
}

func TestParseTypeScript(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "app.ts", `interface User { id: string }

class Service {
  getUser(id: string): User {
    return this.fetch(id);
  }
}

function main() {
  const s = new Service();
  s.getUser("1");
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range res.Symbols {
		names[s.Name] = true
	}
	for _, want := range []string{"User", "Service", "getUser", "main"} {
		if !names[want] {
			t.Fatalf("missing symbol %q in %+v", want, res.Symbols)
		}
	}
}

func TestParsePHP(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "widget.php", `<?php
namespace App;

interface Widget { public function render(); }

class Button implements Widget {
  public function escape(string $s): string { return $s; }
  public function render() {
    Registry::log($this->escape($this->label));
    return $this->escape("x");
  }
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range res.Symbols {
		names[s.Name] = true
	}
	for _, want := range []string{"Widget", "Button", "render"} {
		if !names[want] {
			t.Fatalf("missing symbol %q in %+v", want, res.Symbols)
		}
	}
	// Call edges are class-qualified: $this->escape -> Button::escape and
	// Registry::log keeps its explicit class prefix. Same-named methods of
	// different classes stay distinct graph nodes.
	called := map[string]bool{}
	qualifiedCallers := map[string]bool{}
	for _, e := range res.Edges {
		called[e.Callee] = true
		qualifiedCallers[e.Caller] = true
	}
	if !called["Button::escape"] {
		t.Fatalf("qualified $this->edge missing: %+v", res.Edges)
	}
	if !called["Registry::log"] {
		t.Fatalf("scoped Class::method edge missing: %+v", res.Edges)
	}
	if !qualifiedCallers["Button::render"] {
		t.Fatalf("qualified caller missing: %+v", res.Edges)
	}
	// Symbols carry both names.
	qual := map[string]string{}
	for _, sym := range res.Symbols {
		if sym.Qual != "" {
			qual[sym.Name] = sym.Qual
		}
	}
	if qual["render"] != "Button::render" || qual["escape"] != "Button::escape" {
		t.Fatalf("symbol qualification: %+v", qual)
	}
}

func TestParsePython(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "svc.py", `class Client:
    def fetch(self, url):
        return self.request(url)

def helper():
    c = Client()
    return c.fetch("x")
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range res.Symbols {
		names[s.Name] = true
	}
	if !names["Client"] || !names["fetch"] || !names["helper"] {
		t.Fatalf("missing symbols, got %+v", res.Symbols)
	}
}

func TestUnknownLanguageHashOnly(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "data.bin", "\x00\x01binary")
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) != 0 || res.Lang != "" {
		t.Fatalf("expected hash-only result, got %+v", res)
	}
	if len(res.Hash) != 64 {
		t.Fatalf("bad hash %q", res.Hash)
	}
}

func TestWalkExcludes(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"main.go", "sub/util.go", "node_modules/x.js", ".git/config.go",
	} {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("package x\n"), 0o644)
	}
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f, "node_modules") || strings.Contains(f, ".git") {
			t.Fatalf("excluded dir leaked: %s", f)
		}
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestSplitIdentifier(t *testing.T) {
	cases := map[string]string{
		"AuthorizationError": "authorization error",
		"getUserByID":        "get user by id",
		"snake_case_name":    "snake case name",
		"HTTPServer":         "http server",
		"simple":             "simple",
	}
	for in, want := range cases {
		if got := SplitIdentifier(in); got != want {
			t.Errorf("SplitIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetect(t *testing.T) {
	for f, want := range map[string]string{
		"a.go": "go", "a.php": "php", "a.py": "python", "a.ts": "typescript",
		"a.zz": "",
	} {
		if got := Detect(f); got != want {
			t.Errorf("Detect(%q) = %q, want %q", f, got, want)
		}
	}
}
