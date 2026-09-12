package parse

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// Declaration modifiers and attributes are extracted from the declaration
// node itself — they are declaration data, not documentation, so (unlike
// doc tags) they can never be stale.

// modifierKeywords are anonymous keyword-token types grammars attach to
// definition nodes ("static", "public", …). Anonymous tokens type as their
// own text, so the set doubles as the canonical lowercase spelling.
var modifierKeywords = map[string]bool{
	"public": true, "private": true, "protected": true, "internal": true,
	"static": true, "abstract": true, "final": true, "sealed": true,
	"async": true, "override": true, "virtual": true, "readonly": true,
	"const": true, "required": true, "unsafe": true, "extern": true,
	"synchronized": true, "transient": true, "volatile": true,
	"native": true, "default": true, "declare": true, "new": true,
}

const (
	maxAttrsPerSymbol = 8
	maxAttrChars      = 200
	maxModifiers      = 8
)

// declarationModifiers collects a definition's modifiers (visibility,
// static, abstract, …) and attributes (PHP/C# attributes, Java annotations,
// Python decorators). Modifier nodes and keyword tokens are read from the
// declaration's children; decorators on preceding siblings (Python's
// decorated_definition) are picked up by the sibling walk.
func declarationModifiers(n *gts.Node, lang *gts.Language, src []byte) (mods, attrs []string) {
	seen := map[string]bool{}
	addMod := func(m string) {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || seen[m] || len(mods) >= maxModifiers {
			return
		}
		seen[m] = true
		mods = append(mods, m)
	}
	addAttr := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" || len(attrs) >= maxAttrsPerSymbol {
			return
		}
		if len(a) > maxAttrChars {
			a = a[:maxAttrChars]
		}
		attrs = append(attrs, a)
	}

	collect := func(children func(fn func(*gts.Node))) {
		children(func(c *gts.Node) {
			ct := c.Type(lang)
			switch {
			case ct == "modifiers": // Java wrapper holding keywords + annotations
				for i := 0; i < c.ChildCount(); i++ {
					m := c.Child(i)
					mt := m.Type(lang)
					switch {
					case modifierKeywords[mt]:
						addMod(mt)
					case mt == "marker_annotation" || mt == "annotation":
						addAttr(nodeText(src, m))
					}
				}
			case strings.HasSuffix(ct, "_modifier") || ct == "modifier":
				addMod(nodeText(src, c))
			case ct == "attribute_list":
				collectAttributes(c, lang, src, addAttr)
			case ct == "marker_annotation" || ct == "annotation":
				addAttr(nodeText(src, c))
			case modifierKeywords[ct]:
				addMod(ct)
			}
		})
	}

	collect(func(fn func(*gts.Node)) {
		for i := 0; i < n.ChildCount(); i++ {
			fn(n.Child(i))
		}
	})

	// Python (and any grammar using decorated_definition): decorators are
	// the definition's preceding siblings.
	for sib := n.PrevSibling(); sib != nil && sib.Type(lang) == "decorator"; sib = sib.PrevSibling() {
		addAttr(nodeText(src, sib))
	}
	return mods, attrs
}

// collectAttributes walks an attribute_list, tolerating the PHP
// attribute_group layer between the list and the attributes.
func collectAttributes(list *gts.Node, lang *gts.Language, src []byte, add func(string)) {
	for i := 0; i < list.ChildCount(); i++ {
		c := list.Child(i)
		switch c.Type(lang) {
		case "attribute":
			add(nodeText(src, c))
		case "attribute_group":
			for j := 0; j < c.ChildCount(); j++ {
				g := c.Child(j)
				if g.Type(lang) == "attribute" {
					add(nodeText(src, g))
				}
			}
		}
	}
}

func nodeText(src []byte, n *gts.Node) string {
	start, end := int(n.StartByte()), int(n.EndByte())
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	return string(src[start:end])
}
