// This file defines the language-profile layer: everything language-specific
// about extraction lives behind langProfile, keyed by the grammar's language
// name. Adding a new language means adding a file with a profile (or falling
// back to the common one) — the walk loop in parse.go stays language-neutral.
package parse

import (
	"regexp"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// langProfile captures how a language's AST maps onto skopos' symbol model.
// The common profile covers the convergent naming most grammars use; per
// language files override or add hooks.
type langProfile struct {
	// name is the grammar language name this profile serves.
	name string

	// defs maps definition node types to symbol kinds.
	defs map[string]string

	// containerKinds are symbol kinds that open a type scope: definitions
	// nested inside them get Class::method qualified names.
	containerKinds map[string]bool

	// calls are node types considered call expressions.
	calls map[string]bool

	// selfReceivers maps receiver texts meaning "current instance" to true;
	// calls on them qualify with the enclosing type.
	selfReceivers map[string]bool

	// paramListNodes name the parameter-list node types of a function
	// definition (grammars disagree: formal_parameters vs parameters).
	paramListNodes []string

	// assignmentNodes are node types that may bind a local variable to an
	// instance (assignments, declarations).
	assignmentNodes map[string]bool

	// methodScope is invoked when a function/method definition is entered.
	// It returns the enclosing type for the definition when the language
	// carries it outside the AST nesting (Go receivers) plus extra local
	// variable bindings. Optional.
	methodScope func(def *gts.Node, lang *gts.Language, src []byte) (typeName string, vars map[string]string)

	// nameNodes are leaf node types whose text is a symbol name in this
	// grammar, beyond the shared identifierTypes (e.g. twig names the macro
	// identifier node "method"). Optional.
	nameNodes map[string]bool

	// embeddedSections extracts embedded language chunks from a host
	// document (script/style in Svelte/Vue/HTML) for re-parsing with the
	// matching grammar. Optional.
	embeddedSections func(root *gts.Node, lang *gts.Language, src []byte) []section

	// conditionalDefs lets a profile emit definitions for node types that
	// only sometimes define (JS `const f = () => {}`): the func inspects the
	// node and returns the kind to emit, or ("", false).
	conditionalDefs map[string]func(n *gts.Node, lang *gts.Language) (string, bool)

	// qualifyCallee is an extra hook after generic resolution: it may
	// qualify a callee the generic rules left bare (PHP's `Klass::method`
	// static syntax and `X::class` container idiom). Optional.
	qualifyCallee func(call *gts.Node, lang *gts.Language, src []byte, receiverText, method string) (string, bool)
}

// isName reports whether a node type is a name node for this profile.
func (p *langProfile) isName(nodeType string) bool {
	return identifierTypes[nodeType] || p.nameNodes[nodeType]
}

// profiles maps language names to their profile; registration happens in
// per-language files via init(). Languages without an entry use commonProfile.
var profiles = map[string]*langProfile{}

func registerProfile(p *langProfile) { profiles[p.name] = p }

// profileFor returns the profile for a grammar language name.
func profileFor(lang string) *langProfile {
	if p, ok := profiles[lang]; ok {
		return p
	}
	return commonProfile
}

// commonDefs converges the definition node-type names used across grammars.
var commonDefs = map[string]string{
	// universal-ish
	"function_declaration": "func", "function_definition": "func",
	"function_item":      "func", // rust
	"method_declaration": "method", "method_definition": "method",
	"class_declaration": "class", "class_definition": "class",
	"interface_declaration": "interface",
	"enum_declaration":      "enum", "enum_item": "enum",
	"trait_declaration": "trait",
	"struct_item":       "struct", "struct_specifier": "struct",
	// Go: type_declaration is deliberately absent — type_spec inside it
	// already carries the name (keeping both double-emits the symbol).
	"type_spec": "type", "type_alias_declaration": "type",
	// ruby
	"method": "method", "class": "class", "module": "module",
	// rust
	"trait_item": "trait", "impl_item": "impl",
}

var commonContainerKinds = map[string]bool{
	"class": true, "struct": true, "interface": true, "trait": true,
	"enum": true, "impl": true, "module": true,
}

var commonCalls = map[string]bool{
	"call_expression":          true, // go, js, ts, c, ...
	"call":                     true, // ruby, python
	"function_call":            true,
	"function_call_expression": true, // php: foo()
	"method_call_expression":   true, // php8-style
	"member_call_expression":   true, // php: $obj->method()
	"scoped_call_expression":   true, // php: Class::method()
	"method_invocation":        true, // java
	"invocation_expression":    true, // c#
}

// commonSelfReceivers cover the instance keywords across grammars; harmless
// where unused.
var commonSelfReceivers = map[string]bool{
	"this": true, "$this": true, "self": true, "static": true,
}

// commonProfile is the fallback for every grammar without a specific file.
var commonProfile = &langProfile{
	name:           "*",
	defs:           commonDefs,
	containerKinds: commonContainerKinds,
	calls:          commonCalls,
	selfReceivers:  commonSelfReceivers,
	paramListNodes: []string{"formal_parameters", "parameters"},
	assignmentNodes: map[string]bool{
		"assignment_expression": true, // php: $x = new X()
		"variable_declarator":   true, // ts/js/java: const x = new X()
	},
}

// ---- generic helpers shared by profiles ----

// identifierTypes are node types whose text is a symbol name.
var identifierTypes = map[string]bool{
	"identifier":           true,
	"type_identifier":      true,
	"field_identifier":     true,
	"property_identifier":  true,
	"constant":             true, // ruby
	"name":                 true, // java/php class names
	"package_identifier":   true,
	"namespace_identifier": true,
	// css selectors
	"class_name": true, "id_name": true, "tag_name": true,
}

// typeNodeClasses returns the class of a parameter/`new` type node, "" when
// unresolvable (primitives, unions, intersections are skipped honestly).
func typeNodeClasses(n *gts.Node, lang *gts.Language, src []byte) string {
	switch n.Type(lang) {
	case "named_type", "type_identifier", "generic_type":
		return identifierText(n, lang, src)
	case "nullable_type", "pointer_type":
		// nullable (php/ts), pointer (go *Server)
		if inner := n.NamedChild(0); inner != nil {
			return typeNodeClasses(inner, lang, src)
		}
	case "type":
		// python annotation wrapper; its child is a plain identifier
		if inner := n.NamedChild(0); inner != nil {
			if identifierTypes[inner.Type(lang)] {
				return string(src[inner.StartByte():inner.EndByte()])
			}
			return typeNodeClasses(inner, lang, src)
		}
	}
	return ""
}

// formalParamTypes extracts variable -> class bindings from a definition's
// parameter list, using the profile's parameter-list node names.
func (p *langProfile) formalParamTypes(def *gts.Node, lang *gts.Language, src []byte) map[string]string {
	var params *gts.Node
	for i := 0; i < def.ChildCount(); i++ {
		c := def.Child(i)
		if c == nil {
			continue
		}
		for _, want := range p.paramListNodes {
			if c.Type(lang) == want {
				params = c
				break
			}
		}
		if params != nil {
			break
		}
	}
	if params == nil {
		return nil
	}
	var vars map[string]string
	for i := 0; i < params.NamedChildCount(); i++ {
		par := params.NamedChild(i)
		if par == nil {
			continue
		}
		class, vname := "", ""
		for j := 0; j < par.NamedChildCount(); j++ {
			c := par.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Type(lang) {
			case "variable_name", "identifier", "property_identifier":
				vname = strings.TrimPrefix(strings.TrimPrefix(string(src[c.StartByte():c.EndByte()]), "$"), "...")
			default:
				if t := typeNodeClasses(c, lang, src); t != "" && class == "" {
					class = t
				}
			}
		}
		if class != "" && vname != "" {
			if vars == nil {
				vars = map[string]string{}
			}
			vars[vname] = class
		}
	}
	return vars
}

// newBindingNodes are the `new` expression node types across grammars.
var newBindingNodes = map[string]bool{
	"object_creation_expression": true, // php, java
	"new_expression":             true, // ts/js
}

// newBinding detects a variable bound to a freshly constructed instance in
// an assignment or declaration node, returning variable and class.
func newBinding(n *gts.Node, lang *gts.Language, src []byte) (v, class string, ok bool) {
	var left, right *gts.Node
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch {
		case c.Type(lang) == "variable_name" || (c.Type(lang) == "identifier" && left == nil):
			if left == nil {
				left = c
			}
		case newBindingNodes[c.Type(lang)]:
			right = c
		}
	}
	if left == nil || right == nil {
		return "", "", false
	}
	name := identifierText(right, lang, src)
	if name == "" {
		return "", "", false
	}
	v = strings.TrimPrefix(string(src[left.StartByte():left.EndByte()]), "$")
	return v, name, v != ""
}

var varNameRe = regexp.MustCompile(`^\$?([A-Za-z_][A-Za-z0-9_]*)$`)

// classLiteralRe matches `SomeClass::class` inside a receiver expression
// (PHP container idiom).
var classLiteralRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)::class\b`)

// varBinding is one variable->class binding at a byte offset.
type varBinding struct {
	at    int
	class string
}

// varBindings maps a local variable to its bindings in source order; a call
// resolves the latest binding at or before its own offset (straight-line
// approximation: branches are not tracked).
type varBindings map[string][]varBinding

// resolve returns the class bound to the variable at byte offset at, if any.
func (vb varBindings) resolve(v string, at int) (string, bool) {
	class := ""
	found := false
	for _, b := range vb[v] {
		if b.at <= at {
			class, found = b.class, true
		} else {
			break
		}
	}
	return class, found
}

// collectVarBindings pre-scans a function body: parameter types bind at
// offset 0, then every local `var = new Klass()` (assignment or declarator)
// binds at its offset. Nested function definitions are skipped (own scope).
func (p *langProfile) collectVarBindings(def *gts.Node, lang *gts.Language, src []byte) varBindings {
	vb := varBindings{}
	add := func(v, class string, at int) {
		vb[v] = append(vb[v], varBinding{at, class})
	}
	for v, class := range p.formalParamTypes(def, lang, src) {
		add(v, class, 0)
	}
	var scan func(n *gts.Node)
	scan = func(n *gts.Node) {
		if n == nil {
			return
		}
		if n != def {
			if kind, ok := p.defs[n.Type(lang)]; ok && (kind == "func" || kind == "method") {
				return // nested definition: its own scope
			}
		}
		if p.assignmentNodes[n.Type(lang)] {
			if v, class, ok := newBinding(n, lang, src); ok {
				add(v, class, int(n.StartByte()))
			}
		}
		for i := 0; i < n.ChildCount(); i++ {
			scan(n.Child(i))
		}
	}
	scan(def)
	return vb
}

// identifierText reads the name text of a type-ish node.
func identifierText(n *gts.Node, lang *gts.Language, src []byte) string {
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c != nil && identifierTypes[c.Type(lang)] {
			return string(src[c.StartByte():c.EndByte()])
		}
	}
	if identifierTypes[n.Type(lang)] {
		return string(src[n.StartByte():n.EndByte()])
	}
	return ""
}
