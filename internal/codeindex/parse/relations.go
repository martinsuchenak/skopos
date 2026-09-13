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


// ---- type references ----

// primitiveTypeNames are skipped when recording type references: nobody
// asks "who uses int".
var primitiveTypeNames = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float": true, "float32": true, "float64": true, "double": true,
	"string": true, "bool": true, "boolean": true, "byte": true, "rune": true,
	"any": true, "void": true, "object": true, "mixed": true, "callable": true,
	"iterable": true, "self": true, "static": true, "array": true,
	"error": false, // error is a real interface reference in Go
}

// isPrimitiveTypeName guards type-reference emission.
func isPrimitiveTypeName(n string) bool { return primitiveTypeNames[n] }

// typeRefFunc extracts referenced type names from a type-position node.
func typeRefFunc(deep bool) func(n *gts.Node, lang *gts.Language, src []byte) []string {
	return func(n *gts.Node, lang *gts.Language, src []byte) []string {
		if deep {
			return filterPrimitives(clauseNames(n, lang, src, 2))
		}
		text := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
		if isPrimitiveTypeName(text) {
			return nil
		}
		return []string{text}
	}
}

func filterPrimitives(names []string) []string {
	out := names[:0]
	for _, n := range names {
		if !isPrimitiveTypeName(n) {
			out = append(out, n)
		}
	}
	return out
}

// goTypeRef: type_identifier everywhere except the declared name in a
// type_spec (that is a definition, not a reference).
func goTypeRef(n *gts.Node, lang *gts.Language, src []byte) []string {
	if p := n.Parent(); p != nil && (p.Type(lang) == "type_spec" || p.Type(lang) == "type_alias_declaration") {
		return nil
	}
	text := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
	if isPrimitiveTypeName(text) {
		return nil
	}
	return []string{text}
}

// ---- ruby ----

func rubyRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		"superclass": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			return relations("extends", clauseNames(n, lang, src, 1)...)
		},
	}
}

// includeStyleCalls maps method-call names that declare module inclusion
// (Ruby include/extend/prepend) to a relation kind; the argument is the
// included module.
var includeStyleCalls = map[string]string{
	"include": "uses", "extend": "uses", "prepend": "uses",
}

// ---- rust ----

func rustRelations() map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
	return map[string]func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation{
		// impl Trait for Type — emitted alongside the impl symbol; the
		// walkTree def branch supplies the type as the edge caller.
		"impl_item": func(n *gts.Node, lang *gts.Language, src []byte) []typeRelation {
			seenFor := false
			for i := 0; i < n.ChildCount(); i++ {
				c := n.Child(i)
				if c == nil {
					continue
				}
				switch c.Type(lang) {
				case "for":
					seenFor = true
				case "type_identifier":
					if !seenFor {
						return relations("implements", string(src[c.StartByte():c.EndByte()]))
					}
				}
			}
			return nil
		},
	}
}

// rustImplName returns the TYPE of `impl Trait for Type` as the symbol name.
func rustImplName(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
	seenFor := false
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil {
			continue
		}
		switch c.Type(lang) {
		case "for":
			seenFor = true
		case "type_identifier":
			if seenFor {
				return string(src[c.StartByte():c.EndByte()]), true
			}
		}
	}
	return "", false
}

// ---- imports ----

// importName extracts the module being imported from an import statement.
// Module specifiers are string literals (js/ts/python/go) and win over the
// imported names; name-only imports (php/java) use the identifier path.
func importName(n *gts.Node, lang *gts.Language, src []byte) string {
	var strFound, idFound string
	var walk func(n *gts.Node, d int)
	walk = func(n *gts.Node, d int) {
		if d > 4 {
			return
		}
		switch ct := n.Type(lang); {
		case ct == "string" || ct == "string_" || ct == "raw_string" ||
			ct == "interpreted_string_literal" || ct == "string_literal":
			if strFound == "" {
				strFound = strings.Trim(strings.TrimSpace(string(src[n.StartByte():n.EndByte()])), "\"'`")
			}
		case identifierTypes[ct] || ct == "qualified_name" || ct == "dotted_name":
			if idFound == "" {
				idFound = string(src[n.StartByte():n.EndByte()])
			}
		default:
			for i := 0; i < n.ChildCount(); i++ {
				walk(n.Child(i), d+1)
			}
		}
	}
	walk(n, 0)
	found := strFound
	if found == "" {
		found = idFound
	}
	// Name-only imports join the definitions' short-name namespace; module
	// paths keep their shape.
	if strFound == "" && !strings.ContainsAny(found, "/.") {
		return shortTypeName(found)
	}
	return found
}


// phpInstanceofRef: `$x instanceof Foo` types the target as a bare name
// under a binary_expression; the instanceof token gates the extraction.
func phpInstanceofRef(n *gts.Node, lang *gts.Language, src []byte) []string {
	hasInstanceof := false
	var last string
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil {
			continue
		}
		ct := c.Type(lang)
		if ct == "instanceof" {
			hasInstanceof = true
		}
		if identifierTypes[ct] && ct != "variable_name" {
			last = string(src[c.StartByte():c.EndByte()])
		}
	}
	if !hasInstanceof || last == "" || isPrimitiveTypeName(last) {
		return nil
	}
	return []string{last}
}
