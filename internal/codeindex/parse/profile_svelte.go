// Single-file-component languages (Svelte, Vue, plain HTML): the outer
// grammar is HTML-shaped and leaves <script>/<style> contents as raw_text.
// This profile extracts those sections and re-parses them with the JS/TS and
// CSS grammars, so functions and styles inside components are indexed with
// correct line numbers.
package parse

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

func init() {
	for _, name := range []string{"svelte", "vue", "html"} {
		base := *commonProfile
		base.name = name
		base.embeddedSections = sfcSections
		base.containerKinds = map[string]bool{} // the outer tree defines nothing itself
		base.defs = map[string]string{}
		base.calls = map[string]bool{}
		registerProfile(&base)
	}
	// Blade: {{ expr }} chunks live in php_only nodes as raw PHP expression
	// fragments (no <?php tag); wrap them so the PHP grammar parses cleanly.
	base := *commonProfile
	base.name = "blade"
	base.embeddedSections = bladeSections
	base.containerKinds = map[string]bool{}
	base.defs = map[string]string{}
	base.calls = map[string]bool{}
	registerProfile(&base)
}

// section is one embedded language chunk inside a host document.
type section struct {
	lang       string // grammar name to re-parse with ("javascript", "typescript", "css", "php")
	start      int    // byte offset in the host source
	src        []byte
	lineAdjust int // line delta applied after host offset (e.g. -1 for injected prologues)
}

// sfcSections finds <script ...> and <style> raw_text spans in an HTML-ish
// tree. A lang="ts" attribute on the script tag switches the chunk to the
// TypeScript grammar.
func sfcSections(root *gts.Node, lang *gts.Language, src []byte) []section {
	var out []section
	var walk func(n *gts.Node)
	walk = func(n *gts.Node) {
		if n == nil {
			return
		}
		nt := n.Type(lang)
		if nt == "script_element" || nt == "style_element" {
			chunkLang := "javascript"
			if nt == "style_element" {
				chunkLang = "css"
			}
			// Attributes live on the start_tag; look for lang="ts".
			var attrs *gts.Node
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && strings.HasSuffix(c.Type(lang), "tag") {
					attrs = c
					break
				}
			}
			if attrs != nil {
				attrText := string(src[attrs.StartByte():attrs.EndByte()])
				if strings.Contains(attrText, `lang="ts"`) || strings.Contains(attrText, "lang='ts'") {
					chunkLang = "typescript"
				}
			}
			// The raw_text child holds the content.
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && c.Type(lang) == "raw_text" {
					out = append(out, section{
						lang:  chunkLang,
						start: int(c.StartByte()),
						src:   src[c.StartByte():c.EndByte()],
					})
				}
			}
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

// bladeSections extracts every php_only child (the content of {{ ... }} and
// @directive arguments) as a PHP fragment, wrapped in a <?php prologue so
// the PHP grammar parses it as a program. The prologue is one line, so
// section lines shift by one and are corrected by the caller via
// section.lineAdjust.
func bladeSections(root *gts.Node, lang *gts.Language, src []byte) []section {
	var out []section
	var walk func(n *gts.Node)
	walk = func(n *gts.Node) {
		if n == nil {
			return
		}
		if n.Type(lang) == "php_only" {
			content := src[n.StartByte():n.EndByte()]
			wrapped := append([]byte("<?php "), content...)
			out = append(out, section{lang: "php", start: int(n.StartByte()), src: wrapped})
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

// sectionGrammar resolves a section's grammar name to its language.
func sectionGrammar(name string) *gts.Language {
	switch name {
	case "javascript":
		return grammars.JavascriptLanguage()
	case "typescript":
		return grammars.TypescriptLanguage()
	case "css":
		return grammars.CssLanguage()
	case "php":
		return grammars.PhpLanguage()
	}
	return nil
}
