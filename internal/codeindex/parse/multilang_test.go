package parse

import (
	"os"
	"path/filepath"
	"testing"
)

func parseStr(t *testing.T, name, src string) *FileResult {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	os.WriteFile(p, []byte(src), 0o644)
	res, err := NewExtractor().ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func symbolsByQual(res *FileResult) map[string]Symbol {
	m := map[string]Symbol{}
	for _, s := range res.Symbols {
		key := s.Name
		if s.Qual != "" {
			key = s.Qual
		}
		m[key] = s
	}
	return m
}

func callees(res *FileResult) map[string]bool {
	m := map[string]bool{}
	for _, e := range res.Edges {
		m[e.Callee] = true
	}
	return m
}

// TestProfileGo covers Go receiver-based qualification: methods are
// top-level declarations, so the type scope comes from the receiver, and the
// receiver variable resolves calls inside the body.
func TestProfileGo(t *testing.T) {
	res := parseStr(t, "svc.go", `package svc
type Server struct{ Port int }
func (s *Server) Start() error { return s.listen() }
func (s *Server) listen() error { return nil }
func NewServer(p int) *Server { return &Server{Port: p} }
`)
	syms := symbolsByQual(res)
	if _, ok := syms["Server::Start"]; !ok {
		t.Fatalf("receiver-qualified method missing: %+v", res.Symbols)
	}
	if _, ok := syms["Server::listen"]; !ok {
		t.Fatalf("receiver-qualified method missing: %+v", res.Symbols)
	}
	if _, ok := syms["NewServer"]; !ok {
		t.Fatalf("top-level func missing: %+v", res.Symbols)
	}
	if !callees(res)["Server::listen"] {
		t.Fatalf("s.listen() should resolve via receiver binding: %+v", res.Edges)
	}
}

// TestProfilePython covers the root-node fix (Python's root is `module`,
// which collided with the Ruby module kind and double-qualified classes) and
// annotated-parameter binding.
func TestProfilePython(t *testing.T) {
	res := parseStr(t, "svc.py", `class Service:
    def handle(self, request: Request) -> None:
        request.reply()
        self.done()

    def done(self) -> None: pass
`)
	syms := symbolsByQual(res)
	if _, ok := syms["Service"]; !ok {
		t.Fatalf("class must be top-level (no phantom module, no double qual): %+v", res.Symbols)
	}
	if _, ok := syms["Service::handle"]; !ok {
		t.Fatalf("method qualification missing: %+v", res.Symbols)
	}
	if !callees(res)["Service::done"] {
		t.Fatalf("self.done() should resolve: %+v", res.Edges)
	}
	if !callees(res)["Request::reply"] {
		t.Fatalf("annotated parameter should resolve: %+v", res.Edges)
	}
}

// TestProfileRust covers function_item extraction inside impl blocks.
func TestProfileRust(t *testing.T) {
	res := parseStr(t, "svc.rs", `struct Server { port: u32 }
impl Server {
    fn start(&self) -> u32 { self.listen() }
    fn listen(&self) -> u32 { 0 }
}
`)
	syms := symbolsByQual(res)
	if _, ok := syms["Server::start"]; !ok {
		t.Fatalf("impl fn missing: %+v", res.Symbols)
	}
	if !callees(res)["Server::listen"] {
		t.Fatalf("self.listen() should resolve: %+v", res.Edges)
	}
}

// TestProfileTS covers declaration-form instance binding (const u = new U()).
func TestProfileTS(t *testing.T) {
	res := parseStr(t, "svc.ts", `class UserService {
  get(r: Req): string { return this.format(r.id); }
  format(s: string): string { return s; }
  make(): UserService { return new UserService(); }
  use(): void {
    const u = new UserService();
    u.format("x");
  }
}
`)
	syms := symbolsByQual(res)
	if _, ok := syms["UserService::get"]; !ok {
		t.Fatalf("class method qualification missing: %+v", res.Symbols)
	}
	if !callees(res)["UserService::format"] {
		t.Fatalf("this.format() should resolve: %+v", res.Edges)
	}
	if !callees(res)["UserService::format"] {
		t.Fatalf("const u = new UserService(); u.format() should resolve via declarator binding: %+v", res.Edges)
	}
}

// TestProfileJava mirrors TS: typed parameter + this + new-binding.
func TestProfileJava(t *testing.T) {
	res := parseStr(t, "Svc.java", `public class Svc {
  public void run(Request r) { r.exec(); this.done(); }
  public void done() {}
  public void use() {
    Svc s = new Svc();
    s.done();
  }
}
`)
	if !callees(res)["Request::exec"] || !callees(res)["Svc::done"] {
		t.Fatalf("java qualification: %+v", res.Edges)
	}
}

// TestProfileJS covers arrow/function consts as symbols, uppercase static
// receivers, and module-scope instance bindings.
func TestProfileJS(t *testing.T) {
	res := parseStr(t, "app.js", `const handler = () => { save(); };
const makeThing = function() { return 1; };
class Widget {
  init() { load(); }
  static create() { return new Widget(); }
}
Widget.create();
const u = new Widget();
u.init();
export function exported() {}
`)
	syms := symbolsByQual(res)
	for _, want := range []string{"handler", "makeThing", "exported", "Widget::init", "Widget::create"} {
		if _, ok := syms[want]; !ok {
			t.Fatalf("missing symbol %q: %+v", want, res.Symbols)
		}
	}
	c := callees(res)
	if !c["Widget::create"] {
		t.Fatalf("Widget.create() should qualify statically: %+v", res.Edges)
	}
	if !c["Widget::init"] {
		t.Fatalf("module-scope const u = new Widget(); u.init() should resolve: %+v", res.Edges)
	}
	if c["init"] {
		t.Fatalf("bare init leaked: %+v", res.Edges)
	}
}

// TestProfileTSStatic mirrors JS on the TS grammar; lowercase receivers
// (console) must stay bare.
func TestProfileTSStatic(t *testing.T) {
	res := parseStr(t, "app.ts", `export class Service {
  get(r: User): string { return this.helper(r.id); }
  helper(s: string): string { return s; }
}
Service.get(null);
console.log("x");
`)
	c := callees(res)
	if !c["Service::helper"] || !c["Service::get"] {
		t.Fatalf("TS qualification: %+v", res.Edges)
	}
	if !c["log"] {
		t.Fatalf("console.log must stay bare (lowercase receiver): %+v", res.Edges)
	}
}

// TestProfileCSS: selectors are the stylesheet's definitions.
func TestProfileCSS(t *testing.T) {
	res := parseStr(t, "style.css", `.card { color: red; }
#main .card:hover { margin: 0; }
@media (max-width: 600px) { .card { padding: 2px; } }
`)
	syms := symbolsByQual(res)
	for _, want := range []string{"card", "main"} {
		if _, ok := syms[want]; !ok {
			t.Fatalf("missing selector symbol %q: %+v", want, res.Symbols)
		}
	}
	if syms["card"].Kind != "class" || syms["main"].Kind != "id" {
		t.Fatalf("selector kinds: %+v", res.Symbols)
	}
}

// TestProfileSvelte: <script>/<style> raw_text sections re-parse with the
// JS/TS/CSS grammars, line numbers adjusted to the host file.
func TestProfileSvelte(t *testing.T) {
	res := parseStr(t, "App.svelte", `<script>
import { api } from './api.js';
export function formatUser(u) { return api(name); }
class Store {
  load() { internalHelper(); }
}
function internalHelper() { return 1; }
const store = new Store();
store.load();
</script>
<div class="card">{formatUser(user)}</div>
<style>
.card { color: red; }
</style>
`)
	syms := symbolsByQual(res)
	for _, want := range []string{"formatUser", "internalHelper", "Store::load", "card"} {
		if _, ok := syms[want]; !ok {
			t.Fatalf("missing %q: %+v", want, res.Symbols)
		}
	}
	// Line numbers point into the host file: formatUser on line 3, .card on line 12.
	if syms["formatUser"].Line != 3 {
		t.Fatalf("formatUser line = %d, want 3 (host-relative)", syms["formatUser"].Line)
	}
	if syms["card"].Kind != "class" || syms["card"].Line < 10 || syms["card"].Line > 14 {
		t.Fatalf("css selector: %+v", syms["card"])
	}
	c := callees(res)
	if !c["Store::load"] {
		t.Fatalf("module-scope store binding in section: %+v", res.Edges)
	}
}

// TestProfileSvelteTS: lang="ts" script sections use the TypeScript grammar.
func TestProfileSvelteTS(t *testing.T) {
	res := parseStr(t, "App.svelte", `<script lang="ts">
export function get(r: User): string { return this.helper(r); }
</script>
`)
	syms := symbolsByQual(res)
	if _, ok := syms["get"]; !ok {
		t.Fatalf("ts section symbols: %+v", res.Symbols)
	}
}

// TestProfileVue: same machinery for .vue single-file components.
func TestProfileVue(t *testing.T) {
	res := parseStr(t, "Widget.vue", `<template><div/></template>
<script>
export default {
  methods: {
    save() { this.load(); },
    load() {},
  },
};
</script>
<style scoped>
.a { margin: 0; }
</style>
`)
	syms := symbolsByQual(res)
	for _, want := range []string{"save", "load", "a"} {
		if _, ok := syms[want]; !ok {
			t.Fatalf("missing %q: %+v", want, res.Symbols)
		}
	}
}
