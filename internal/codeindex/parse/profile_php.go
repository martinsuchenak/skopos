// PHP-specific extraction: static Klass::method() syntax and the
// SomeClass::class container idiom. Everything else PHP needs is covered by
// the common profile ($this receivers, formal_parameters, assignments).
package parse

import (
	"regexp"

	gts "github.com/odvcencio/gotreesitter"
)

func init() {
	base := *commonProfile // copy
	base.name = "php"
	base.qualifyCallee = phpQualifyCallee
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
