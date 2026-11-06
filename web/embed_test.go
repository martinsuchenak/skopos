package web

import (
	"bytes"
	"os"
	"regexp"
	"testing"
)

// The release pipeline builds the frontend before running tests, so a missing
// or truncated bundle must fail there. Local checkouts without a build skip —
// compiling still works via the committed dist/.gitkeep placeholder.
func TestVerifyAssets(t *testing.T) {
	if _, err := StaticFiles.ReadFile("dist/app.js"); err != nil {
		t.Skip("frontend not built locally (run task frontend-build); release builds gate this")
	}
	if err := VerifyAssets(); err != nil {
		t.Fatal(err)
	}
}

// TestTemplatesAvoidXHtml guards against reintroducing the html directive:
// the Alpine CSP build (@alpinejs/csp) PROHIBITS it — it errors on sight, so
// a template using it renders an empty element with no other failure signal.
// Rendered HTML must be injected imperatively from first-party TS (see
// reloadInboxItem in web/src/main.ts) using server-sanitized content only.
func TestTemplatesAvoidXHtml(t *testing.T) {
	raw, err := TemplateFiles.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("reading base.html: %v", err)
	}
	// Comments may mention the directive by name; only live markup counts.
	stripped := commentRe.ReplaceAll(raw, nil)
	if bytes.Contains(stripped, []byte("x-html")) {
		t.Fatal("base.html uses the html directive, which the Alpine CSP build prohibits (renders empty; inject via x-ref + innerHTML from TS instead)")
	}
}

var commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// TestLockfileHasOneCodeMirrorCore guards the inbox editor: CodeMirror checks
// extensions with instanceof, so a nested second copy of a core package (bun
// update once left @codemirror/state 6.7.5 under every language package
// beside a top-level 6.7.6) makes the editor throw "Unrecognized extension
// value" and no text can be entered. vite.config.ts dedupes at build time;
// this keeps the lockfile honest too. Fix: rm -rf node_modules bun.lock &&
// bun install.
func TestLockfileHasOneCodeMirrorCore(t *testing.T) {
	raw, err := os.ReadFile("bun.lock")
	if err != nil {
		t.Fatalf("reading bun.lock: %v", err)
	}
	for _, m := range nestedCoreRe.FindAllSubmatch(raw, -1) {
		t.Errorf("bun.lock has a nested %s (under %s): two copies in the bundle break CodeMirror", m[2], m[1])
	}
}

var nestedCoreRe = regexp.MustCompile(`"([^"\s]+)/(@codemirror/(?:state|view|language)|@lezer/(?:common|highlight))":`)
