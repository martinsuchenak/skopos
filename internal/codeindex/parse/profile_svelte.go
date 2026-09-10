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
}

// section is one embedded language chunk inside a host document.
type section struct {
	lang  string // grammar name to re-parse with ("javascript", "typescript", "css")
	start int    // byte offset in the host source
	src   []byte
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

// sectionGrammar resolves a section's grammar name to its language.
func sectionGrammar(name string) *gts.Language {
	switch name {
	case "javascript":
		return grammars.JavascriptLanguage()
	case "typescript":
		return grammars.TypescriptLanguage()
	case "css":
		return grammars.CssLanguage()
	}
	return nil
}
