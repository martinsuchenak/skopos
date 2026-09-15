// PHP-specific extraction: static Klass::method() syntax and the
// SomeClass::class container idiom. Everything else PHP needs is covered by
// the common profile ($this receivers, formal_parameters, assignments).
package parse

import (
	"regexp"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

func init() {
	base := *commonProfile // copy
	base.name = "php"
	base.defs = cloneDefs(commonDefs)
	base.qualifyCallee = phpQualifyCallee
	base.typeRefNodes = map[string]func(n *gts.Node, lang *gts.Language, src []byte) []string{
		"named_type":                       typeRefFunc(false),
		"binary_expression":                phpInstanceofRef,
		"class_constant_access_expression": phpClassConstantRef,
		"scoped_constant_access":           phpClassConstantRef,
	}
	base.relationNodes = phpRelations()
	base.importNodes = map[string]bool{"namespace_use_declaration": true}
	base.defs["property_declaration"] = "property"
	base.defs["const_declaration"] = "const"
	base.defs["enum_case"] = "case"
	base.defName = phpDefName
	registerProfile(&base)
}

// phpQualifyCallee qualifies callees the generic rules left bare:
//
//	X::class  — app(Repo::class)->save() → Repo::save (the literal names the type)
//	X::method — Klass::static() kept with its class prefix
func phpQualifyCallee(call *gts.Node, lang *gts.Language, src []byte, receiverText, method string) (string, bool) {
	if receiverText == "" {
		return "", false
	}
	if m := classLiteralRe.FindStringSubmatch(receiverText); m != nil {
		return m[1] + "::" + method, true
	}
	// Explicit Class::method receiver: the receiver text itself is
	// `Klass` and the call text contains the `::` form.
	if m := staticRe.FindStringSubmatch(string(src[call.StartByte():call.EndByte()])); m != nil && m[2] == method {
		return m[1] + "::" + method, true
	}
	return "", false
}

// staticRe matches an explicit `Klass::method(` inside a call's text.
var staticRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)::([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// phpDefName reads names the generic childName cannot reach: property
// elements wrap variable names, const elements wrap names.
func phpDefName(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
	switch n.Type(lang) {
	case "property_declaration":
		if el := phpDescend(n, lang, "property_element"); el != nil {
			if v := phpDescend(el, lang, "variable_name"); v != nil {
				return strings.TrimPrefix(string(src[v.StartByte():v.EndByte()]), "$"), true
			}
		}
	case "const_declaration":
		if el := phpDescend(n, lang, "const_element"); el != nil {
			if nm := phpDescend(el, lang, "name"); nm != nil {
				return string(src[nm.StartByte():nm.EndByte()]), true
			}
		}
	}
	return "", false
}

func phpDescend(n *gts.Node, lang *gts.Language, typ string) *gts.Node {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.Type(lang) == typ {
			return c
		}
	}
	return nil
}

// phpClassConstantRef turns bare `SomeClass::class` mentions (argument
// position: is_a($x, Repo::class), arrays of class names) into
// type-reference edges on the named class — previously only the receiver
// position (app(Repo::class)->save()) was captured, leaving
// argument-position mentions unfindable.
func phpClassConstantRef(n *gts.Node, lang *gts.Language, src []byte) []string {
	text := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
	if m := classLiteralRe.FindStringSubmatch(text); m != nil {
		// Only the ::class literal itself, not ::CONSTANT accesses.
		if m[0] == text {
			return []string{m[1]}
		}
	}
	return nil
}
