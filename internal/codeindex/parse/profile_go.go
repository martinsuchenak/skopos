// Go-specific extraction: methods are top-level declarations whose type
// scope comes from the receiver (`func (s *Server) Start()`), not from AST
// nesting. The receiver also binds the variable inside the body, so
// `s.listen()` resolves to Server::listen.
package parse

import (
	gts "github.com/odvcencio/gotreesitter"
)

func init() {
	base := *commonProfile
	base.name = "go"
	base.defs = cloneDefs(commonDefs)
	base.methodScope = goMethodScope
	base.typeRefNodes = map[string]func(n *gts.Node, lang *gts.Language, src []byte) []string{
		"type_identifier": goTypeRef,
	}
	base.relationNodes = goRelations()
	base.importNodes = map[string]bool{"import_spec": true}
	base.defs["const_spec"] = "const"
	if base.conditionalDefs == nil {
		base.conditionalDefs = map[string]func(n *gts.Node, lang *gts.Language) (string, bool){}
	}
	base.conditionalDefs["field_declaration"] = func(n *gts.Node, lang *gts.Language) (string, bool) {
		hasName, hasType := false, false
		for i := 0; i < n.NamedChildCount(); i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			if c.Type(lang) == "field_identifier" {
				hasName = true
			} else {
				hasType = true
			}
		}
		if hasName && hasType {
			return "field", true
		}
		return "", false // embedded types are recorded as embeds relations
	}
	registerProfile(&base)
}

// goMethodScope reads a method_declaration's receiver parameter:
// (s *Server) → typeName "Server", vars {"s": "Server"}.
func goMethodScope(def *gts.Node, lang *gts.Language, src []byte) (string, map[string]string) {
	if def.Type(lang) != "method_declaration" {
		return "", nil
	}
	var receiver *gts.Node
	for i := 0; i < def.ChildCount(); i++ {
		if c := def.Child(i); c != nil && c.Type(lang) == "parameter_list" {
			receiver = c
			break
		}
	}
	if receiver == nil {
		return "", nil
	}
	par := receiver.NamedChild(0)
	if par == nil {
		return "", nil
	}
	vname, class := "", ""
	for j := 0; j < par.NamedChildCount(); j++ {
		c := par.NamedChild(j)
		if c == nil {
			continue
		}
		if c.Type(lang) == "identifier" {
			vname = string(src[c.StartByte():c.EndByte()])
		} else if t := typeNodeClasses(c, lang, src); t != "" {
			class = t
		}
	}
	if class == "" {
		return "", nil
	}
	if vname == "" {
		return class, nil
	}
	return class, map[string]string{vname: class}
}
