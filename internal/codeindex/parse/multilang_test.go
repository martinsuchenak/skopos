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
