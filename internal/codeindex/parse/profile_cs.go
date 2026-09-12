// C#-specific extraction. The grammar orders method children as
// [modifiers, return-type identifier, name identifier, parameter_list], so
// the generic first-identifier name lookup picks the return type — the name
// is the LAST direct identifier instead. Parameters have the same shape
// (type identifier, name identifier), and PascalCase receivers are types by
// convention (`UserService.Create()`), mirroring the JS static heuristic.
package parse

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

func init() {
	base := *commonProfile
	base.name = "c_sharp"
	base.defs = map[string]string{
		"class_declaration":       "class",
		"struct_declaration":      "struct",
		"interface_declaration":   "interface",
		"record_declaration":      "class",
		"enum_declaration":        "enum",
		"method_declaration":      "method",
		"constructor_declaration": "method",
		"property_declaration":    "property",
	}
	base.paramListNodes = []string{"parameter_list"}
	base.implicitSelfCalls = true
	base.defName = csDefName
	base.paramTypes = csParamTypes
	base.qualifyCallee = csQualifyCallee
	registerProfile(&base)
}

// csDefName: the definition's name is the last direct identifier child
// (modifiers and the return type precede it).
func csDefName(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
	name := ""
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c != nil && c.Type(lang) == "identifier" {
			name = string(src[c.StartByte():c.EndByte()])
		}
	}
	return name, name != ""
}

// csParamTypes: a C# parameter is (type, name) — either may be a plain
// identifier; the name is the LAST identifier, the type is what precedes it.
func csParamTypes(def *gts.Node, lang *gts.Language, src []byte) map[string]string {
	var params *gts.Node
	for i := 0; i < def.ChildCount(); i++ {
		if c := def.Child(i); c != nil && c.Type(lang) == "parameter_list" {
			params = c
			break
		}
	}
	if params == nil {
		return nil
	}
	var vars map[string]string
	for i := 0; i < params.NamedChildCount(); i++ {
		par := params.NamedChild(i)
		if par == nil || par.Type(lang) != "parameter" {
			continue
		}
		var ids []string
		other := ""
		for j := 0; j < par.ChildCount(); j++ {
			c := par.Child(j)
			if c == nil {
				continue
			}
			if c.Type(lang) == "identifier" {
				ids = append(ids, string(src[c.StartByte():c.EndByte()]))
			} else if t := typeNodeClasses(c, lang, src); t != "" {
				other = t
			}
		}
		if len(ids) == 0 {
			continue
		}
		name := ids[len(ids)-1]
		class := other
		if len(ids) >= 2 {
			class = ids[len(ids)-2] // (Type name) spelling
		}
		if class != "" && name != "" {
			if vars == nil {
				vars = map[string]string{}
			}
			vars[name] = class
		}
	}
	return vars
}

// csQualifyCallee: a plain PascalCase receiver is a type by convention
// (UserService.Create, User.New). camelCase receivers (locals, fields) fall
// through to variable bindings or stay bare.
func csQualifyCallee(call *gts.Node, lang *gts.Language, src []byte, receiverText, method string) (string, bool) {
	if receiverText == "" || strings.ContainsAny(receiverText, ".()[]$") {
		return "", false
	}
	r := []rune(receiverText)
	if len(r) == 0 || r[0] < 'A' || r[0] > 'Z' {
		return "", false
	}
	return receiverText + "::" + method, true
}
