package parse

import gts "github.com/odvcencio/gotreesitter"

func init() {
	base := *commonProfile
	base.name = "java"
	base.defs = cloneDefs(commonDefs)
	base.typeRefNodes = map[string]func(n *gts.Node, lang *gts.Language, src []byte) []string{
		"type_identifier": typeRefFunc(false),
	}
	base.relationNodes = javaRelations()
	base.importNodes = map[string]bool{"import_declaration": true}
	base.defs["field_declaration"] = "field"
	base.defs["variable_declarator"] = "field"
	base.defName = javaDefName
	registerProfile(&base)
}


// javaDefName reads the variable name for field declarations (the generic
// first-identifier lookup would return the type).
func javaDefName(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
	if n.Type(lang) == "field_declaration" {
		if d := phpDescend(n, lang, "variable_declarator"); d != nil {
			for i := 0; i < d.ChildCount(); i++ {
				if c := d.Child(i); c != nil && c.Type(lang) == "identifier" {
					return string(src[c.StartByte():c.EndByte()]), true
				}
			}
		}
	}
	return "", false
}
