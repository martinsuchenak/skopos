package cards

import (
	"fmt"
	"maps"
	"strings"
)

// A2UI 1.0 (a2ui.org): the protocol version and the catalog every surface
// uses. A host draws the basic catalog's components with its own.
const (
	A2UIVersion  = "1.0"
	BasicCatalog = "https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json"
)

// Component is one A2UI component: id, component (its catalog name) and
// its properties, flat as the protocol writes them.
type Component map[string]any

// surface collects a surface's components. Ids are generated in order, so
// the same state builds the same JSON.
type surface struct {
	components []Component
	n          int
}

func (s *surface) add(kind string, props Component) string {
	s.n++
	id := fmt.Sprintf("c%d", s.n)
	c := Component{"id": id, "component": kind}
	maps.Copy(c, props)
	s.components = append(s.components, c)
	return id
}

// text is a Text component: Markdown the host renders (and sanitises).
func (s *surface) text(markdown string) string {
	return s.add("Text", Component{"text": markdown})
}

// caption is a Text in the caption style.
func (s *surface) caption(markdown string) string {
	return s.add("Text", Component{"text": markdown, "variant": "caption"})
}

func (s *surface) card(child string) string {
	return s.add("Card", Component{"child": child})
}

func (s *surface) column(children ...string) string {
	return s.add("Column", Component{"children": children})
}

// row lays its children out side by side, centred on one line.
func (s *surface) row(children ...string) string {
	return s.add("Row", Component{"children": children, "align": "center"})
}

// icon is one of the basic catalog's icons (Icon's name enum).
func (s *surface) icon(name string) string {
	return s.add("Icon", Component{"name": name})
}

// section is a part of the card: a Card holding a heading and the parts.
func (s *surface) section(title string, parts ...string) string {
	return s.card(s.column(append([]string{s.text("#### " + title)}, parts...)...))
}

// headline is the card's title with an icon, and a line under it ("" =
// none).
func (s *surface) headline(icon, title, sub string) string {
	parts := []string{s.row(s.icon(icon), s.text("### "+title))}
	if sub != "" {
		parts = append(parts, s.caption(sub))
	}
	return s.column(parts...)
}

// bind is a value read from the surface's data model (and written back by an
// input).
func bind(path string) map[string]any { return map[string]any{"@path": path} }

// button is a Button with a Text label; pressing it sends the action event
// with the context (literals, or bind() paths the host resolves).
func (s *surface) button(label, variant, action string, context map[string]any) string {
	child := s.add("Text", Component{"text": label})
	return s.add("Button", Component{"child": child, "variant": variant,
		"action": map[string]any{"event": map[string]any{"name": action, "context": context}}})
}

type tab struct{ title, child string }

func (s *surface) tabs(ts []tab) string {
	list := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		list = append(list, map[string]any{"title": t.title, "child": t.child})
	}
	return s.add("Tabs", Component{"tabs": list})
}

// createSurface is an envelope's A2UI messages: deleteSurface, then
// createSurface with every component (the root a column of the given
// children). A surface id may be created only once in a renderer's life
// unless it is deleted first, and every card of an item has the same id:
// deleting first lets a host keep one renderer and apply each card as it
// comes. A renderer that has not seen the surface takes the delete as
// nothing to do. dataModel is the inputs' starting values (nil = none).
func (s *surface) createSurface(surfaceID string, children []string, dataModel map[string]any) []any {
	root := Component{"id": "root", "component": "Column", "children": children, "align": "stretch"}
	comps := append([]Component{root}, s.components...)
	create := map[string]any{
		"surfaceId":  surfaceID,
		"catalogId":  BasicCatalog,
		"components": comps,
	}
	if dataModel != nil {
		create["dataModel"] = dataModel
	}
	return []any{
		map[string]any{"version": A2UIVersion, "deleteSurface": map[string]any{"surfaceId": surfaceID}},
		map[string]any{"version": A2UIVersion, "createSurface": create},
	}
}

// clip cuts text a person reads on a card to n bytes on a rune boundary.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " …"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// firstLine is a text's first non-empty line.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// indent puts text under a list item: every line after the first starts
// with the item's indent.
func indent(s, pad string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+pad)
}
