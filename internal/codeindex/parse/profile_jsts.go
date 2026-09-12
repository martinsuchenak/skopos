// JavaScript/TypeScript specifics: arrow/function consts become function
// symbols, `Class.static()` receivers qualify (uppercase-initial identifier
// is the languages' static/namespace convention), and module-scope `const`
// bindings are visible inside functions (handled by the walk).
package parse

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

func init() {
	for _, name := range []string{"javascript", "typescript", "tsx"} {
		base := *commonProfile
		base.name = name
		base.conditionalDefs = map[string]func(n *gts.Node, lang *gts.Language) (string, bool){
			"variable_declarator": jsConditionalDefs,
		}
		base.qualifyCallee = jsQualifyCallee
		registerProfile(&base)
	}
}

// jsConditionalDefs: `const f = () => {}` / `const f = function(){}` emit a
// func symbol named by the declarator.
func jsConditionalDefs(n *gts.Node, lang *gts.Language) (string, bool) {
	var isFn bool
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type(lang) {
		case "arrow_function", "function_expression", "function_declaration":
			isFn = true
		}
	}
	if !isFn {
		return "", false
	}
	return "func", true
}

// jsQualifyCallee: a plain uppercase-initial receiver (`Widget.create()`,
// `Promise.all()`) is a class or namespace by convention — qualify the
// callee. Lowercase receivers (`console.log`, `u.init`) fall through to the
// generic variable-binding rules.
func jsQualifyCallee(call *gts.Node, lang *gts.Language, src []byte, receiverText, method string) (string, bool) {
	if receiverText == "" || strings.ContainsAny(receiverText, ".()[]$") {
		return "", false
	}
	r := []rune(receiverText)
	if len(r) == 0 || r[0] < 'A' || r[0] > 'Z' {
		return "", false
	}
	return receiverText + "::" + method, true
}
