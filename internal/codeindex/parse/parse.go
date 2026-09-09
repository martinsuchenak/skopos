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
	"strings"
	"time"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// DefaultTimeout is the per-file parse budget. Files that exceed it are still
// parsed via tree-sitter error recovery and flagged (the measured pathological
// case was an 11s heredoc-heavy PHP file).
const DefaultTimeout = 2 * time.Second

// Symbol is a definition extracted from one file.
type Symbol struct {
	Name      string `json:"name"`
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

// callTypes: node types considered call expressions (callee = first
// identifier-ish descendant).
var callTypes = map[string]bool{
	"call_expression": true, "call": true, "function_call": true,
	"method_invocation": true, " invocation_expression": true,
	"invocation_expression": true, "call_expression_function": false,
}

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
	sum := sha256.Sum256(src)
	res := &FileResult{
		Path: path,
		Hash: hex.EncodeToString(sum[:]),
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
		node   *gts.Node
		caller string
	}
	stack := []frame{{root, ""}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.node == nil {
			continue
		}
		n := f.node
		nt := n.Type(lang)

		caller := f.caller
		if kind, ok := defKinds[nt]; ok {
			if kind == "type" {
				// refine Go-style type specs: type X struct{...} / interface{...}
				if hasChildOfType(n, lang, "struct_type") {
					kind = "struct"
				} else if hasChildOfType(n, lang, "interface_type") {
					kind = "interface"
				}
			}
			if name, ok := childName(n, lang, src); ok && name != "_" {
				sig := signature(src, int(n.StartByte()), int(n.EndByte()))
				res.Symbols = append(res.Symbols, Symbol{
					Name: name, Kind: kind,
					Line:      int(n.StartPoint().Row) + 1,
					StartByte: int(n.StartByte()), EndByte: int(n.EndByte()),
					Signature: sig, Lang: res.Lang,
				})
				caller = name
			}
		} else if callTypes[nt] {
			if callee, ok := calleeName(n, lang, src); ok && callee != "" {
				res.Edges = append(res.Edges, Edge{
					Caller: f.caller, Callee: callee, Kind: "call",
					Line: int(n.StartPoint().Row) + 1,
				})
			}
		}

		// push children in reverse so traversal is source order
		k := n.ChildCount()
		for i := k - 1; i >= 0; i-- {
			stack = append(stack, frame{n.Child(i), caller})
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
// else first identifier-ish descendant (bounded depth).
func calleeName(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
	// Method calls are (operand . name) under the "function" field: the
	// rightmost identifier is the callee name (db.Ping -> Ping), so search
	// named children from the right.
	f := n
	if fn := n.ChildByFieldName("function", lang); fn != nil {
		f = fn
	}
	var last string
	found := false
	for i := 0; i < f.NamedChildCount(); i++ {
		c := f.NamedChild(i)
		if c != nil && identifierTypes[c.Type(lang)] {
			last = string(src[c.StartByte():c.EndByte()])
			found = true
		}
	}
	if found {
		return last, true
	}
	if f != n && identifierTypes[f.Type(lang)] {
		return string(src[f.StartByte():f.EndByte()]), true
	}
	return "", false
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
}

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
		if Detect(p) != "" {
			files = append(files, p)
		}
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
