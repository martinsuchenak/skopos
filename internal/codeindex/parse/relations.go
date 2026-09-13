package parse

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// Type relationships (extends / implements / trait use / embedding) are
// recorded as edges like calls and instantiations, so who-calls, impact,
// and dead-code see them: a base class "calls" from every subclass, and a
// parent change impacts its whole hierarchy.

// typeRelation is one declared relationship: the declaring type is the
// edge's caller (filled in by walkTree from the enclosing frame).
type typeRelation struct {
	kind string // extends | implements | uses | embeds
	name string
}

// clauseNames collects identifier-ish descendant texts from a relation
// clause (base lists, interface lists, type lists). Bounded depth: the
// grammars nest at most one container level (java type_list, cs/ts lists).
func clauseNames(n *gts.Node, lang *gts.Language, src []byte, depth int) []string {
	var out []string
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		ct := c.Type(lang)
		if identifierTypes[ct] || ct == "qualified_name" {
			out = append(out, string(src[c.StartByte():c.EndByte()]))
		} else if depth > 0 {
			out = append(out, clauseNames(c, lang, src, depth-1)...)
		}
	}
	return out
}

func relations(kind string, names ...string) []typeRelation {
	out := make([]typeRelation, 0, len(names))
	for _, nm := range names {
		out = append(out, typeRelation{kind: kind, name: nm})
	}
	return out
}

// phpRelations maps PHP's clause nodes.
func phpRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		// class X extends Base / interface I extends Iface
		"base_clause": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("extends", clauseNames(n, lang, src, 1)...)
		},
		"class_interface_clause": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("implements", clauseNames(n, lang, src, 1)...)
		},
		// `use CacheTrait;` inside a class body
		"use_declaration": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("uses", clauseNames(n, lang, src, 1)...)
		},
	}
}

// jstsRelations: tree-sitter-javascript/typescript exposes dedicated
// extends_clause and implements_clause nodes under class_heritage.
func jstsRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		"extends_clause": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("extends", clauseNames(n, lang, src, 2)...)
		},
		"implements_clause": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("implements", clauseNames(n, lang, src, 2)...)
		},
	}
}

// csRelations: `class X : Base, IFace` — one base_list; the C# convention
// (interfaces are I-prefixed) separates interface from class parents.
func csRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		"base_list": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			var out []typeRelation
			for _, nm := range clauseNames(n, lang, src, 1) {
				kind := "extends"
				if strings.HasPrefix(nm, "I") && len(nm) > 1 && nm[1] >= 'A' && nm[1] <= 'Z' {
					kind = "implements"
				}
				out = append(out, typeRelation{kind: kind, name: nm})
			}
			return out
		},
	}
}

// javaRelations: superclass / super_interfaces nodes.
func javaRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		"superclass": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("extends", clauseNames(n, lang, src, 1)...)
		},
		"super_interfaces": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("implements", clauseNames(n, lang, src, 2)...)
		},
	}
}

// pythonRelations: class Foo(Base, Mixin) — argument_list is also a call's
// argument list, so the parent must be a class definition.
func pythonRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		"argument_list": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			if p := n.Parent(); p == nil || p.Type(lang) != "class_definition" {
				return nil
			}
			return relations("extends", clauseNames(n, lang, src, 0)...)
		},
	}
}

// goRelations: struct embedding — a field with a type and no name embeds it.
func goRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		"field_declaration": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			if p := n.Parent(); p == nil || p.Type(lang) != "field_declaration_list" {
				return nil
			}
			name := ""
			for i := 0; i < n.NamedChildCount(); i++ {
				c := n.NamedChild(i)
				if c == nil {
					continue
				}
				switch ct := c.Type(lang); {
				case ct == "field_identifier" || ct == "identifier":
					return nil // named field: not an embedding
				case identifierTypes[ct] || ct == "qualified_type" || ct == "pointer_type" || ct == "generic_type":
					if name == "" {
						name = string(src[c.StartByte():c.EndByte()])
					}
				}
			}
			if name == "" {
				return nil
			}
			return relations("embeds", strings.TrimPrefix(name, "*"))
		},
	}
}
