// Package parse extracts symbols and reference edges from source files using
// the pure-Go tree-sitter runtime (gotreesitter). It is the only package that
// imports gotreesitter, so the dependency stays a replaceable leaf.
package parse

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// DefaultTimeout is the per-file parse budget. Files that exceed it are still
// parsed via tree-sitter error recovery and flagged (the measured pathological
// case was an 11s heredoc-heavy PHP file).
const DefaultTimeout = 2 * time.Second

// Symbol is a definition extracted from one file. Qual carries the
// type-qualified name ("Class::method") for definitions nested in a type;
// empty when the definition is top-level.
type Symbol struct {
	Name      string `json:"name"`
	Qual      string `json:"qual,omitempty"`
	Kind      string `json:"kind"` // func, method, class, interface, struct, type, enum, trait
	Line      int    `json:"line"` // 1-based
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Signature string `json:"signature,omitempty"` // first source line of the definition, capped
	Lang      string `json:"lang,omitempty"`
}

// Edge is a heuristic reference from one symbol to a name (call or use).
// Name-based only — tree-sitter is syntactic; resolution happens at query time.
type Edge struct {
	Caller string `json:"caller,omitempty"` // enclosing definition ("" = file scope)
	Callee string `json:"callee"`
	Kind   string `json:"kind"` // call | ref
	Line   int    `json:"line"`
}

// FileResult is the full parse output for one file.
type FileResult struct {
	Path    string   `json:"path"`
	Hash    string   `json:"hash"` // sha256 of content
	Lang    string   `json:"lang"`
	Err     bool     `json:"err"` // tree had error nodes (recovered)
	Symbols []Symbol `json:"symbols"`
	Edges   []Edge   `json:"edges"`
}

// defKinds maps definition-ish node types (converged naming across grammars)
// to symbol kinds. Extraction is heuristic by design; per-language refinements
// can specialize later without changing the storage contract.
var defKinds = map[string]string{
	// universal-ish
	"function_declaration": "func", "function_definition": "func",
	"method_declaration": "method", "method_definition": "method",
	"class_declaration": "class", "class_definition": "class",
	"interface_declaration": "interface",
	"enum_declaration":      "enum", "enum_item": "enum",
	"trait_declaration": "trait",
	"struct_item":       "struct", "struct_specifier": "struct",
	"type_spec": "type", "type_alias_declaration": "type",
	// ruby
	"method": "method", "class": "class", "module": "module",
	// rust
	"trait_item": "trait", "impl_item": "impl",
}

// containerKinds are defKinds values that open a type scope: definitions
// nested inside them get qualified names (Class::method).
var containerKinds = map[string]bool{
	"class": true, "struct": true, "interface": true, "trait": true,
	"enum": true, "impl": true, "module": true,
}

// classLiteralRe matches `SomeClass::class` inside a receiver expression
// (PHP container idiom; harmless elsewhere).
var classLiteralRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)::class\b`)

// identifierTypes are node types whose text is a symbol name.
var identifierTypes = map[string]bool{
	"identifier":           true,
	"type_identifier":      true,
	"field_identifier":     true,
	"property_identifier":  true,
	"constant":             true, // ruby
	"name":                 true, // java/php class names
	"field_declaration":    false,
	"package_identifier":   true,
	"namespace_identifier": true,
}

// callTypes: node types considered call expressions (callee = rightmost
// identifier-ish descendant under the "function" field).
var callTypes = map[string]bool{
	"call_expression":          true, // go, js, ts, c, ...
	"call":                     true, // ruby, python
	"function_call":            true, // elixir-ish
	"function_call_expression": true, // php: foo()
	"method_call_expression":   true, // php8-style
	"member_call_expression":   true, // php: $obj->method()
	"scoped_call_expression":   true, // php: Class::method()
	"method_invocation":        true, // java
	"invocation_expression":    true, // c#
}

// ExtractorVersion changes whenever extraction logic changes; it is mixed
// into the content hash so already-indexed files re-extract after upgrades.
const ExtractorVersion = "5"

// Extractor parses files with a shared parser per language.
type Extractor struct {
	timeout time.Duration
	parsers map[string]*gts.Parser
}

func NewExtractor() *Extractor {
	return &Extractor{timeout: DefaultTimeout, parsers: map[string]*gts.Parser{}}
}

func (e *Extractor) SetTimeout(d time.Duration) { e.timeout = d }

// Detect returns the language name for a filename, or "" when unsupported.
func Detect(filename string) string {
	if entry := grammars.DetectLanguage(filename); entry != nil {
		return entry.Name
	}
	return ""
}

// ParseFile reads and parses one file. Unknown languages yield a hash-only
// result with no symbols.
func (e *Extractor) ParseFile(path string) (*FileResult, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return e.ParseBytes(path, src)
}

// ParseBytes parses in-memory content.
func (e *Extractor) ParseBytes(path string, src []byte) (*FileResult, error) {
	sum := sha256.New()
	sum.Write([]byte("skopos-extractor:" + ExtractorVersion + "\x00"))
	sum.Write(src)
	res := &FileResult{
		Path: path,
		Hash: hex.EncodeToString(sum.Sum(nil)),
		Lang: Detect(path),
	}
	if res.Lang == "" {
		return res, nil
	}
	entry := grammars.DetectLanguage(path)
	if entry == nil || entry.Language == nil {
		return res, nil
	}

	parser, ok := e.parsers[res.Lang]
	if !ok {
		parser = gts.NewParser(entry.Language())
		parser.SetTimeoutMicros(uint64(e.timeout.Microseconds()))
		e.parsers[res.Lang] = parser
	}
	tree, err := parser.Parse(src)
	if err != nil || tree == nil {
		// Not parsed: keep hash-only result; flag so callers can decide.
		res.Err = true
		return res, nil
	}
	root := tree.RootNode()
	lang := entry.Language()
	res.Err = root.HasErrorOrMissing()

	type frame struct {
		node     *gts.Node
		caller   string      // enclosing definition (qualified when nested in a type)
		typeName string      // enclosing type name, if any
		vars     varBindings // local variable -> class bindings, offset-indexed
	}
	stack := []frame{{root, "", "", nil}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.node == nil {
			continue
		}
		n := f.node
		nt := n.Type(lang)

		caller := f.caller
		typeName := f.typeName
		vars := f.vars
		if kind, ok := defKinds[nt]; ok {
			// Function/method bodies get a fresh variable-type scope: declared
			// parameter types plus $var = new Klass() assignments, recorded
			// with their byte offsets so each call resolves the binding that
			// precedes it in source order.
			if kind == "func" || kind == "method" {
				vars = collectVarBindings(n, lang, src)
			}
			if kind == "type" {
				// refine Go-style type specs: type X struct{...} / interface{...}
				if hasChildOfType(n, lang, "struct_type") {
					kind = "struct"
				} else if hasChildOfType(n, lang, "interface_type") {
					kind = "interface"
				}
			}
			if name, ok := childName(n, lang, src); ok && name != "_" {
				qual := ""
				if typeName != "" {
					qual = typeName + "::" + name
				}
				sig := signature(src, int(n.StartByte()), int(n.EndByte()))
				res.Symbols = append(res.Symbols, Symbol{
					Name: name, Qual: qual, Kind: kind,
					Line:      int(n.StartPoint().Row) + 1,
					StartByte: int(n.StartByte()), EndByte: int(n.EndByte()),
					Signature: sig, Lang: res.Lang,
				})
				if qual != "" {
					caller = qual
				} else {
					caller = name
				}
				if containerKinds[kind] {
					typeName = name // nested defs now qualify against this type
				}
			}
		} else if callTypes[nt] {
			if callee, ok := calleeName(n, lang, src, typeName, vars); ok && callee != "" {
				res.Edges = append(res.Edges, Edge{
					Caller: f.caller, Callee: callee, Kind: "call",
					Line: int(n.StartPoint().Row) + 1,
				})
			}
		}

		// push children in reverse so traversal is source order
		k := n.ChildCount()
		for i := k - 1; i >= 0; i-- {
			stack = append(stack, frame{n.Child(i), caller, typeName, vars})
		}
	}
	return res, nil
}

// childName finds the first identifier-ish child's text.
func childName(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		if identifierTypes[c.Type(lang)] {
			return string(src[c.StartByte():c.EndByte()]), true
		}
		// one level down covers e.g. Go's method_declaration > receiver is
		// skipped and name found later; also (type: (type_identifier) name: ...)
		for j := 0; j < c.NamedChildCount(); j++ {
			g := c.NamedChild(j)
			if g != nil && identifierTypes[g.Type(lang)] {
				return string(src[g.StartByte():g.EndByte()]), true
			}
		}
	}
	return "", false
}

// hasChildOfType reports whether any direct child has the given node type.
func hasChildOfType(n *gts.Node, lang *gts.Language, typ string) bool {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.Type(lang) == typ {
			return true
		}
	}
	return false
}

// calleeName extracts the callee of a call node: prefer field "function",
// else first identifier-ish descendant (bounded depth). Calls through
// $this/self/static/this are qualified with the enclosing type, and explicit
// Class::method calls keep their class prefix, so same-named methods of
// different types stay distinct graph nodes.
func calleeName(n *gts.Node, lang *gts.Language, src []byte, typeName string, vars varBindings) (string, bool) {
	f := n
	if fn := n.ChildByFieldName("function", lang); fn != nil {
		f = fn
	}
	var last, first string
	var receiverText string
	found := false
	for i := 0; i < f.NamedChildCount(); i++ {
		c := f.NamedChild(i)
		if c == nil {
			continue
		}
		ct := c.Type(lang)
		if ct == "arguments" {
			continue
		}
		// Object of a member/scoped access (variable_name, name, call...).
		if !found && first == "" {
			first = strings.TrimSpace(string(src[c.StartByte():c.EndByte()]))
			receiverText = first
		}
		if identifierTypes[ct] {
			last = string(src[c.StartByte():c.EndByte()])
			found = true
		}
	}
	if !found {
		if f != n && identifierTypes[f.Type(lang)] {
			return string(src[f.StartByte():f.EndByte()]), true
		}
		return "", false
	}
	// Implicit dispatch on the current instance: qualify with the type.
	switch strings.TrimPrefix(first, "$") {
	case "this", "self", "static":
		if typeName != "" {
			return typeName + "::" + last, true
		}
		return last, true
	}
	// Locally-typed variable: $obj = new User() or handle(Request $r) — the
	// binding is syntactic (assignment or signature), so qualify with it.
	if vars != nil && receiverText != "" {
		if m := varNameRe.FindStringSubmatch(receiverText); m != nil {
			if class, bound := vars.resolve(m[1], int(n.StartByte())); bound {
				return class + "::" + last, true
			}
		}
	}
	// Container/factory idiom: the receiver expression names the type via a
	// `SomeClass::class` literal (e.g. app(UserRepo::class)->save()). The
	// literal is authoritative, so qualify the callee with it.
	if m := classLiteralRe.FindStringSubmatch(receiverText); m != nil {
		return m[1] + "::" + last, true
	}
	// Explicit receiver that is itself a plain identifier (Class::method in
	// PHP / Namespace.method elsewhere): keep the receiver as a prefix.
	if first != "" && strings.Contains(string(src[n.StartByte():n.EndByte()]), first+"::"+last) {
		return first + "::" + last, true
	}
	return last, true
}

// signature returns the first source line of a definition, capped.
func signature(src []byte, start, end int) string {
	e := start
	for e < end && e < len(src) && src[e] != '\n' {
		e++
	}
	sig := strings.TrimSpace(string(src[start:e]))
	if len(sig) > 120 {
		sig = sig[:117] + "..."
	}
	return sig
}

// DefaultExcludes are directory names skipped while walking.
var DefaultExcludes = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"bin": true, "build": true, "out": true, "target": true, ".cache": true,
	"__pycache__": true, ".venv": true, "venv": true, ".idea": true, ".vscode": true,
	".yarn": true, ".next": true, ".nuxt": true, "coverage": true, "__snapshots__": true,
	".skopos": true,
}

// MaxFileSize skips files larger than this (bundled/minified artifacts like
// yarn releases would otherwise flood the index with junk symbols).
const MaxFileSize = 1 << 20 // 1 MiB

// Walk returns candidate source files under root (DetectLanguage != "").
func Walk(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if DefaultExcludes[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if Detect(p) == "" {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > MaxFileSize {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return files, nil
}

// SplitIdentifier breaks camelCase / snake_case / kebab-case identifiers into
// lowercase subtokens for full-text indexing ("AuthorizationError" ->
// "authorization error").
func SplitIdentifier(name string) string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(name)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			cur = append(cur, r)
		case r >= 'A' && r <= 'Z':
			// flush before an uppercase that starts a new word: previous char
			// is a lowercase/digit, OR it follows an uppercase run and the
			// next char is lowercase (HTTPServer -> HTTP | Server).
			if len(cur) > 0 {
				prev := cur[len(cur)-1]
				prevLower := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
				nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
				if prevLower || nextLower {
					flush()
				}
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return strings.Join(parts, " ")
}

// formalParamTypes extracts variable -> class bindings from a function's
// declared parameter types (PHP/TS shape: formal_parameters > parameter >
// (type, variable)). Nullable types unwrap; unions/intersections and
// primitives are skipped — only a single named type binds.
func formalParamTypes(def *gts.Node, lang *gts.Language, src []byte) map[string]string {
	var params *gts.Node
	for i := 0; i < def.ChildCount(); i++ {
		if c := def.Child(i); c != nil && c.Type(lang) == "formal_parameters" {
			params = c
			break
		}
	}
	if params == nil {
		return nil
	}
	var vars map[string]string
	for i := 0; i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		class := ""
		vname := ""
		for j := 0; j < p.NamedChildCount(); j++ {
			c := p.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Type(lang) {
			case "named_type", "type_identifier", "generic_type":
				class = identifierText(c, lang, src)
			case "nullable_type":
				if inner := c.NamedChild(0); inner != nil {
					class = identifierText(inner, lang, src)
				}
			case "variable_name", "identifier", "property_identifier":
				vname = strings.TrimPrefix(strings.TrimPrefix(string(src[c.StartByte():c.EndByte()]), "$"), "...")
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

// newBinding detects `$var = new Klass(...)` assignments, returning the
// variable name (without $) and the class name. Children are scanned
// positionally — the PHP grammar carries no left/right field names here.
func newBinding(n *gts.Node, lang *gts.Language, src []byte) (v, class string, ok bool) {
	var left, right *gts.Node
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type(lang) {
		case "variable_name":
			if left == nil {
				left = c
			}
		case "object_creation_expression":
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
	bs := vb[v]
	class := ""
	found := false
	for _, b := range bs {
		if b.at <= at {
			class, found = b.class, true
		} else {
			break
		}
	}
	return class, found
}

// collectVarBindings pre-scans a function body: parameter types bind at
// offset 0, then every `$var = new Klass(...)` binds at its assignment
// offset. Nested function definitions are skipped (own scope).
func collectVarBindings(def *gts.Node, lang *gts.Language, src []byte) varBindings {
	vb := varBindings{}
	add := func(v, class string, at int) {
		vb[v] = append(vb[v], varBinding{at, class})
	}
	for v, class := range formalParamTypes(def, lang, src) {
		add(v, class, 0)
	}
	var scan func(n *gts.Node)
	scan = func(n *gts.Node) {
		if n == nil {
			return
		}
		if n != def {
			if kind, ok := defKinds[n.Type(lang)]; ok && (kind == "func" || kind == "method") {
				return // nested definition: its own scope
			}
		}
		if n.Type(lang) == "assignment_expression" {
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
