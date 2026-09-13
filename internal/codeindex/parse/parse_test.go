package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestParsePHPClassLiteralReceiver(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "handler.php", `<?php
class Handler {
  public function run(): void {
    app(Repo::class)->save();
    Container::make(Mailer::class)->send();
    $this->local();
  }
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	called := map[string]bool{}
	for _, e := range res.Edges {
		called[e.Callee] = true
	}
	// The SomeClass::class literal names the receiver type: qualify.
	if !called["Repo::save"] || !called["Mailer::send"] {
		t.Fatalf("::class receiver edges missing: %+v", res.Edges)
	}
	if !called["Handler::local"] {
		t.Fatalf("$this edge missing: %+v", res.Edges)
	}
}

func TestParsePHPInstanceCallResolution(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "instances.php", `<?php
class User { public function getName(): string { return "x"; } }
class Repo { public function find(): ?User { return null; } }
class Handler {
  public function viaNew(): void {
    $obj = new User();
    $obj->getName();
  }
  public function viaParam(Request $r, ?User $maybe): void {
    $r->all();
    $maybe->getName();
  }
  public function rebinding(): void {
    $o = new User();
    $o = new Repo();
    $o->find();
  }
  public function unknown($x): void {
    $x->anything();
  }
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	called := map[string]bool{}
	for _, e := range res.Edges {
		called[e.Callee] = true
	}
	if !called["User::getName"] {
		t.Fatalf("$obj = new User(); $obj->getName() should resolve: %+v", res.Edges)
	}
	if !called["Request::all"] {
		t.Fatalf("typed parameter Request $r should resolve: %+v", res.Edges)
	}
	if !called["Repo::find"] {
		t.Fatalf("last assignment should win ($o = new Repo()): %+v", res.Edges)
	}
	if !called["anything"] {
		t.Fatalf("untyped receiver must stay bare: %+v", res.Edges)
	}
}

func TestDocExtractionPHP(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "test.php", `<?php
/**
 * Sends the password reset email to a user.
 *
 * Long-running: queues the job and returns immediately.
 *
 * @param int $retryAttempts attempts before giving up (stale doc)
 * @param string $email (stale doc)
 * @return bool (stale doc)
 * @throws MailException when the transport fails
 * @deprecated use Mailer::queueReset() instead
 */
#[Route('/reset')]
function sendPasswordResetMail($user, $transport): void {
    $transport->send($user);
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var sym *Symbol
	for i, s := range res.Symbols {
		if s.Name == "sendPasswordResetMail" {
			sym = &res.Symbols[i]
		}
	}
	if sym == nil {
		t.Fatal("symbol not extracted")
	}
	// Declaration wins: signature comes from the code, not the doc.
	if sym.Signature == "" || !strings.Contains(sym.Signature, "function sendPasswordResetMail") {
		t.Fatalf("signature from declaration: %q", sym.Signature)
	}
	for _, want := range []string{
		"Sends the password reset email to a user.",
		"Long-running: queues the job and returns immediately.",
		"@throws MailException when the transport fails",
		"@deprecated use Mailer::queueReset() instead",
	} {
		if !strings.Contains(sym.Doc, want) {
			t.Errorf("doc missing %q; got:\n%s", want, sym.Doc)
		}
	}
	// Stale signature tags are dropped.
	if strings.Contains(sym.Doc, "@param") || strings.Contains(sym.Doc, "@return") {
		t.Errorf("doc keeps signature tags (declaration must win):\n%s", sym.Doc)
	}
}

func TestDocExtractionGo(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "a.go", `package p

// SplitIdentifier breaks camelCase identifiers into
// lowercase subtokens.
//
// Returns the input unchanged when there is nothing to split.
func SplitIdentifier(name string) string { return name }
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) == 0 {
		t.Fatal("no symbols")
	}
	s := res.Symbols[0]
	if !strings.Contains(s.Doc, "breaks camelCase identifiers") || !strings.Contains(s.Doc, "nothing to split") {
		t.Fatalf("Go doc comment not captured: %q", s.Doc)
	}
}

func TestDocExtractionPythonDocstring(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "a.py", `def load_config(path):
    """Load the app configuration from a YAML file.

    Returns an empty config when the file is missing.
    """
    return {}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) == 0 {
		t.Fatal("no symbols")
	}
	s := res.Symbols[0]
	if !strings.Contains(s.Doc, "Load the app configuration from a YAML file.") {
		t.Fatalf("docstring not captured: %q", s.Doc)
	}
}

func TestDocExtractionJSDoc(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "a.js", `/**
 * Validates a reset token before consumption.
 * @param {string} token (stale doc)
 * @throws {Error} on malformed token
 */
export function validateResetToken(tok) {}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var sym *Symbol
	for i, s := range res.Symbols {
		if s.Name == "validateResetToken" {
			sym = &res.Symbols[i]
		}
	}
	if sym == nil {
		t.Fatal("symbol not extracted")
	}
	if !strings.Contains(sym.Doc, "Validates a reset token before consumption.") || !strings.Contains(sym.Doc, "@throws") {
		t.Fatalf("JSDoc not captured as expected: %q", sym.Doc)
	}
	if strings.Contains(sym.Doc, "@param") {
		t.Errorf("stale @param kept: %q", sym.Doc)
	}
}

func TestModifierExtraction(t *testing.T) {
	cases := []struct {
		name, file, src, sym string
		wantMods []string
		wantAttrSubstrings []string
	}{
		{
			name: "php attributes and modifiers",
			file: "t.php", sym: "handle",
			src: "<?php\nclass A {\n  #[Route('/reset', name: 'reset')]\n  public static function handle($r): void {}\n}\n",
			wantMods: []string{"public", "static"},
			wantAttrSubstrings: []string{"Route('/reset', name: 'reset')"},
		},
		{
			name: "php abstract protected",
			file: "t.php", sym: "make",
			src: "<?php\nabstract class A {\n  abstract protected function make();\n}\n",
			wantMods: []string{"abstract", "protected"},
		},
		{
			name: "typescript accessibility and keywords",
			file: "t.ts", sym: "run",
			src: "class A {\n  private static async run(): Promise<void> {}\n}\n",
			wantMods: []string{"private", "static", "async"},
		},
		{
			name: "csharp modifier nodes and attributes",
			file: "t.cs", sym: "Get",
			src: "public class A {\n  [HttpGet(\"/users\")]\n  public static async Task<int> Get() { return 1; }\n}\n",
			wantMods: []string{"public", "static", "async"},
			wantAttrSubstrings: []string{"HttpGet(\"/users\")"},
		},
		{
			name: "python decorators",
			file: "t.py", sym: "list_users",
			src: "@app.route('/users', methods=['GET'])\n@cache.cached(60)\ndef list_users():\n    return []\n",
			wantAttrSubstrings: []string{"@app.route('/users'", "@cache.cached(60)"},
		},
		{
			name: "java annotations in modifiers node",
			file: "t.java", sym: "run",
			src: "public class A {\n  @Override\n  @SuppressWarnings(\"x\")\n  public static void run() {}\n}\n",
			wantMods: []string{"public", "static"},
			wantAttrSubstrings: []string{"@Override", "@SuppressWarnings(\"x\")"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExtractor()
			p := writeTemp(t, tc.file, tc.src)
			res, err := e.ParseFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var sym *Symbol
			for i, s := range res.Symbols {
				if s.Name == tc.sym {
					sym = &res.Symbols[i]
				}
			}
			if sym == nil {
				t.Fatalf("symbol %s not extracted: %+v", tc.sym, res.Symbols)
			}
			if len(sym.Modifiers) != len(tc.wantMods) {
				t.Fatalf("modifiers = %v, want %v", sym.Modifiers, tc.wantMods)
			}
			for i, m := range tc.wantMods {
				if sym.Modifiers[i] != m {
					t.Fatalf("modifiers = %v, want %v", sym.Modifiers, tc.wantMods)
				}
			}
			for _, want := range tc.wantAttrSubstrings {
				found := false
				for _, a := range sym.Attrs {
					if strings.Contains(a, want) {
						found = true
					}
				}
				if !found {
					t.Fatalf("attr %q missing in %v", want, sym.Attrs)
				}
			}
		})
	}
}

func TestDocNotAttachedAcrossBlankLine(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "a.go", "package p\n\n// Unrelated trailing note.\n\nfunc Far() {}\n")
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) == 0 {
		t.Fatal("no symbols")
	}
	if res.Symbols[0].Doc != "" {
		t.Fatalf("comment attached across a blank line: %q", res.Symbols[0].Doc)
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	// "žžž" is 2 bytes per rune: cutting at 5 must back off to 4, not split.
	if got := Truncate("žžžž", 5); got != "žž" {
		t.Fatalf("rune-unsafe truncation: %q", got)
	}
	if got := Truncate("žžžž", 8); got != "žžžž" {
		t.Fatalf("no-op truncation changed input: %q", got)
	}
	if got := Truncate("ascii", 3); got != "asc" {
		t.Fatalf("ascii truncation: %q", got)
	}
	// A doc block whose cap lands mid-rune stays valid UTF-8.
	long := strings.Repeat("ž", 600) // 1200 bytes > maxDocBytes
	var sym Symbol
	sym.Doc = long
	_ = sym
	doc := Truncate(long, maxDocBytes)
	if !utf8.ValidString(doc) {
		t.Fatal("truncated doc is not valid UTF-8")
	}
}

func TestNewExpressionRecordedAsEdge(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "t.php", `<?php
use App\Services\CacheService;

function makeCache() {
    $a = new \App\Services\CacheService();
    $a->warm();
    return $a;
}

function plainNew() {
    return new CacheService();
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	news := map[string]bool{} // callee -> seen
	for _, e := range res.Edges {
		if e.Kind == "new" && e.Callee == "CacheService" {
			news[e.Caller] = true
		}
	}
	if !news["makeCache"] || !news["plainNew"] {
		t.Fatalf("new-expression edges missing (qualified and/or bare): %+v", res.Edges)
	}
	// The method call through the binding still resolves to the qualified name.
	foundWarm := false
	for _, e := range res.Edges {
		if e.Kind == "call" && e.Callee == "CacheService::warm" {
			foundWarm = true
		}
	}
	if !foundWarm {
		t.Fatalf("binding-qualified call missing: %+v", res.Edges)
	}
}

func TestNewExpressionJS(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "t.js", "function boot() {\n  const w = new Widget();\n  w.render();\n}\n")
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Edges {
		if e.Kind == "new" && e.Callee == "Widget" && e.Caller == "boot" {
			found = true
		}
	}
	if !found {
		t.Fatalf("JS new edge missing: %+v", res.Edges)
	}
}


func TestTypeRelations(t *testing.T) {
	cases := []struct {
		name, file, src, caller string
		want                    map[string]string // callee -> kind
	}{
		{
			name: "php extends implements trait",
			file: "t.php", caller: "Foo",
			src: "<?php\nclass Foo extends Base implements Iface1, Iface2 {\n  use CacheTrait;\n}\n",
			want: map[string]string{"Base": "extends", "Iface1": "implements", "Iface2": "implements", "CacheTrait": "uses"},
		},
		{
			name: "typescript extends implements",
			file: "t.ts", caller: "Foo",
			src: "class Foo extends Base implements Iface {}\n",
			want: map[string]string{"Base": "extends", "Iface": "implements"},
		},
		{
			name: "csharp base list",
			file: "t.cs", caller: "Foo",
			src: "public class Foo : Base, IFace {}\n",
			want: map[string]string{"Base": "extends", "IFace": "implements"},
		},
		{
			name: "java superclass interfaces",
			file: "t.java", caller: "Foo",
			src: "public class Foo extends Base implements Iface {}\n",
			want: map[string]string{"Base": "extends", "Iface": "implements"},
		},
		{
			name: "python base classes",
			file: "t.py", caller: "Foo",
			src: "class Foo(Base, Mixin):\n    pass\n",
			want: map[string]string{"Base": "extends", "Mixin": "extends"},
		},
		{
			name: "go struct embedding",
			file: "t.go", caller: "Foo",
			src: "package p\ntype Foo struct {\n\tBase\n\tName string\n}\n",
			want: map[string]string{"Base": "embeds"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExtractor()
			p := writeTemp(t, tc.file, tc.src)
			res, err := e.ParseFile(p)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, e := range res.Edges {
				if e.Caller == tc.caller {
					switch e.Kind {
					case "extends", "implements", "uses", "embeds":
						got[e.Callee] = e.Kind
					}
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("relations = %v, want %v (all edges: %+v)", got, tc.want, res.Edges)
			}
			for callee, kind := range tc.want {
				if got[callee] != kind {
					t.Fatalf("%s: got kind %q, want %q", callee, got[callee], kind)
				}
			}
		})
	}
}

func TestTypeReferenceEdges(t *testing.T) {
	e := NewExtractor()
	p := writeTemp(t, "t.php", `<?php
class Handler {
  private CacheService $cache;
  public function __construct(Logger $log) {}
  public function run(HttpRequest $r): Response {
    if ($r instanceof UploadRequest) {}
    try {} catch (HttpException $e) {}
  }
}
`)
	res, err := e.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == "references" && strings.HasPrefix(e.Caller, "Handler") {
			refs[e.Callee] = true
		}
	}
	for _, want := range []string{"CacheService", "Logger", "HttpRequest", "Response", "UploadRequest", "HttpException"} {
		if !refs[want] {
			t.Errorf("type reference %s missing (got %v)", want, refs)
		}
	}
}

func TestFieldConstEnumSymbols(t *testing.T) {
	cases := []struct {
		file, src, name, kind string
	}{
		{"t.php", "<?php\nclass A {\n  private CacheService $cache;\n  const MAX = 5;\n}\n", "cache", "property"},
		{"t.php", "<?php\nclass A { const MAX_RETRIES = 5; }\n", "MAX_RETRIES", "const"},
		{"t.php", "<?php\nenum Status { case Active; }\n", "Active", "case"},
		{"t.ts", "class A { cache: CacheService; }\n", "cache", "property"},
		{"t.ts", "enum Color { Red = 1, Green = 2 }\n", "Red", "case"},
		{"t.cs", "public class A { private CacheService cache; }\n", "cache", "field"},
		{"t.java", "public class A { private CacheService cache; }\n", "cache", "field"},
		{"t.go", "package p\nconst MaxRetries = 5\n", "MaxRetries", "const"},
		{"t.go", "package p\ntype S struct { Name string }\n", "Name", "field"},
	}
	for _, tc := range cases {
		t.Run(tc.file+":"+tc.name, func(t *testing.T) {
			res, err := NewExtractor().ParseFile(writeTemp(t, tc.file, tc.src))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range res.Symbols {
				if s.Name == tc.name {
					if s.Kind != tc.kind {
						t.Fatalf("%s: kind %s, want %s", tc.name, s.Kind, tc.kind)
					}
					return
				}
			}
			t.Fatalf("%s not extracted: %+v", tc.name, res.Symbols)
		})
	}
}

func TestLocalJSConstIsNotAField(t *testing.T) {
	res, err := NewExtractor().ParseFile(writeTemp(t, "t.js", "function boot() {\n  const w = new Widget();\n  w.render();\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Symbols {
		if s.Name == "w" {
			t.Fatalf("local const emitted as a field symbol: %+v", s)
		}
	}
}

func TestImportEdges(t *testing.T) {
	cases := []struct {
		file, src, want string
	}{
		{"t.php", "<?php\nuse App\\Services\\CacheService;\nfunction f() {}\n", "CacheService"},
		{"t.go", "package p\nimport \"github.com/org/repo/pkg\"\n", "github.com/org/repo/pkg"},
		{"t.py", "import os\nfrom lib.helpers import thing\n", "lib.helpers"},
		{"t.ts", "import { x } from \"./util\";\n", "./util"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			res, err := NewExtractor().ParseFile(writeTemp(t, tc.file, tc.src))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range res.Edges {
				if e.Kind == "import" && strings.Contains(e.Callee, tc.want) {
					return
				}
			}
			t.Fatalf("import %q missing: %+v", tc.want, res.Edges)
		})
	}
}

func TestRubyAndRustRelations(t *testing.T) {
	res, err := NewExtractor().ParseFile(writeTemp(t, "t.rb", "class Foo < Base\n  include CacheTrait\nend\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range res.Edges {
		if e.Caller == "Foo" {
			got[e.Callee] = e.Kind
		}
	}
	if got["Base"] != "extends" || got["CacheTrait"] != "uses" {
		t.Fatalf("ruby relations: %v", got)
	}

	res2, err := NewExtractor().ParseFile(writeTemp(t, "t.rs", "struct UserService;\nimpl Repo for UserService { }\n"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res2.Edges {
		if e.Caller == "UserService" && e.Callee == "Repo" && e.Kind == "implements" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rust impl-trait relation missing: %+v", res2.Edges)
	}
}
