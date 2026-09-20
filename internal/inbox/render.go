package inbox

import (
	"bytes"
	"html"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Markdown rendering for the dashboard (docs/design/inbox.md, decision 8).
// Storage and the agent-facing surfaces (MCP, CLI) keep the raw markdown;
// only the read path for the UI renders it, server-side, in two layers:
//
//  1. goldmark with the GFM extension (tables, strikethrough, linkify) and
//     the DEFAULT renderer config — html.WithUnsafe is deliberately NOT set,
//     so raw HTML in the markdown source is escaped, never passed through.
//  2. bluemonday's UGC policy, which strips anything hostile that survives
//     rendering (javascript:/data: link schemes, event-handler attributes,
//     script/style/iframe elements).
//
// The output is pinned by adversarial tests in render_test.go; the dashboard
// CSP (script-src 'self') remains the backstop, not the only layer.
var (
	renderOnce sync.Once
	md         goldmark.Markdown
	sanitizer  *bluemonday.Policy
)

func initRenderers() {
	renderOnce.Do(func() {
		md = goldmark.New(goldmark.WithExtensions(extension.GFM))
		sanitizer = bluemonday.UGCPolicy()
	})
}

// RenderMarkdown converts item markdown to dashboard-safe HTML.
func RenderMarkdown(source string) string {
	initRenderers()
	var buf bytes.Buffer
	if err := md.Convert([]byte(source), &buf); err != nil {
		// Rendering should never fail for in-memory output; degrade to fully
		// escaped source rather than showing nothing (or unsanitized HTML).
		return html.EscapeString(source)
	}
	return sanitizer.Sanitize(buf.String())
}
