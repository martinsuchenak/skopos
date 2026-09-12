// Twig templates: macros are the language's named definitions, and output
// expressions `{{ helper(x) }}` carry real call edges to helpers and macros.
// Filters (`|escape`) are Twig builtins and deliberately not edges.
package parse

func init() {
	base := *commonProfile
	base.name = "twig"
	base.defs = map[string]string{
		"macro_statement": "func",
	}
	base.calls = map[string]bool{
		"function_call": true,
	}
	// The twig grammar names the macro's identifier node "method" and the
	// callee "function_identifier" — leaf name nodes this profile recognizes.
	base.nameNodes = map[string]bool{
		"method":              true,
		"function_identifier": true,
	}
	base.containerKinds = map[string]bool{}
	base.selfReceivers = map[string]bool{}
	base.paramListNodes = nil
	base.assignmentNodes = map[string]bool{}
	registerProfile(&base)
}
