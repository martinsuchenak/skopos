package web

import (
	"bytes"
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
