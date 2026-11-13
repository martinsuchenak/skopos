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

// A tracker row's state is a word the script picks; the row partial draws
// its mark, the stylesheet colours it and the script names it for screen
// readers. A state missing from one of the three shows as an empty circle
// with no label (the second-opinion step added "flag").
func TestStageStatesHaveMarkStyleAndLabel(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		return string(raw)
	}
	set := func(src string, re *regexp.Regexp) map[string]bool {
		out := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			out[m[1]] = true
		}
		return out
	}
	script := read("src/main.ts")
	marks := set(read("templates/partials.html"), regexp.MustCompile(`s\.state === '(\w+)'`))
	styles := set(read("src/style.css"), regexp.MustCompile(`(?m)^\.stage-(\w+) \.stage-mark \{`))
	words := regexp.MustCompile(`const words: Record<string, string> = \{([^}]*)\}`).FindStringSubmatch(script)
	if words == nil {
		t.Fatal("stageStateLabel's words not found in main.ts")
	}
	labels := set(words[1], regexp.MustCompile(`(\w+): '`))
	// Every state the script assigns to a row.
	used := set(script, regexp.MustCompile(`\bstate = '(\w+)'`))
	for s := range set(script, regexp.MustCompile(`curState = '(\w+)'`)) {
		used[s] = true
	}
	if len(used) < 5 || len(marks) < 5 {
		t.Fatalf("found too few states to check: used %v, marks %v", used, marks)
	}
	for s := range used {
		if !labels[s] {
			t.Errorf("state %q has no screen-reader label in stageStateLabel", s)
		}
		// "up" (not reached yet) is the plain empty circle.
		if s != "up" && (!marks[s] || !styles[s]) {
			t.Errorf("state %q: mark in the row partial %v, .stage-%s .stage-mark style %v", s, marks[s], s, styles[s])
		}
	}
}
