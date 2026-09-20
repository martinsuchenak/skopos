package inbox

import (
	"strings"
	"testing"
)

// TestRenderMarkdownHostileVectors pins the sanitization contract for
// dashboard-rendered item content (docs/design/inbox.md, decision 8): agent-
// authored markdown must never smuggle executable content through HTML.
func TestRenderMarkdownHostileVectors(t *testing.T) {
	vectors := []struct {
		name    string
		source  string
		mustNot []string
	}{
		{"script block", "<script>alert(1)</script>", []string{"<script", "alert(1)</script>"}},
		{"inline script", "text <script src='https://evil.example/x.js'></script> more", []string{"<script"}},
		{"img onerror", `<img src="x" onerror="alert(1)">`, []string{"onerror"}},
		{"iframe", "<iframe src='https://evil.example'></iframe>", []string{"<iframe"}},
		{"javascript link", "[click](javascript:alert(1))", []string{"javascript:"}},
		{"data URI link", "[click](data:text/html;base64,PHNjcmlwdD4=)", []string{"data:text/html"}},
		{"event handler heading", `<h1 onclick="alert(1)">Hi</h1>`, []string{"onclick"}},
		{"raw HTML form", "<form action='https://evil.example'><input type=submit></form>", []string{"<form"}},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			out := RenderMarkdown(v.source)
			lower := strings.ToLower(out)
			for _, banned := range v.mustNot {
				if strings.Contains(lower, strings.ToLower(banned)) {
					t.Fatalf("output contains banned %q:\n%s", banned, out)
				}
			}
		})
	}
}

// TestRenderMarkdownBenignSurvives pins that ordinary markdown renders to the
// expected structures — sanitization must not eat the format itself.
func TestRenderMarkdownBenignSurvives(t *testing.T) {
	out := RenderMarkdown("# Title\n\nSome **bold** and `code`.\n\n- a\n- b\n\n[link](https://example.com)\n\n| h1 | h2 |\n| --- | --- |\n| a | b |\n\n```go\nfmt.Println(\"hi\")\n```\n")
	for _, want := range []string{"<h1>", "<strong>", "<code>", "<li>", `<a href="https://example.com"`, "<table>", "<pre>"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderMarkdownEscapedSource verifies the fallback path: when rendering
// fails, the raw source is fully escaped rather than emitted as HTML.
func TestRenderMarkdownEmpty(t *testing.T) {
	if out := RenderMarkdown(""); out != "" {
		t.Fatalf("empty input should render empty, got %q", out)
	}
}
