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

// ExtractorVersion changes whenever extraction logic changes; it is mixed
// into the content hash so already-indexed files re-extract after upgrades.
const ExtractorVersion = "8"

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

// Extractor parses files with a shared parser per language.
type Extractor struct {
	timeout time.Duration
	parsers map[string]*gts.Parser
}

func NewExtractor() *Extractor {
	return &Extractor{timeout: DefaultTimeout, parsers: map[string]*gts.Parser{}}
}

func (e *Extractor) SetTimeout(d time.Duration) { e.timeout = d }

// parserFor returns a shared, timeout-configured parser per language.
func (e *Extractor) parserFor(name string, lang *gts.Language) *gts.Parser {
	if p, ok := e.parsers[name]; ok {
		return p
	}
	p := gts.NewParser(lang)
	p.SetTimeoutMicros(uint64(e.timeout.Microseconds()))
	e.parsers[name] = p
	return p
}

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

	parser := e.parserFor(res.Lang, entry.Language())
	tree, err := parser.Parse(src)
	if err != nil || tree == nil {
		// Not parsed: keep hash-only result; flag so callers can decide.
		res.Err = true
		return res, nil
	}
	root := tree.RootNode()
	lang := entry.Language()
	res.Err = root.HasErrorOrMissing()

	walkTree(profileFor(res.Lang), root, lang, src, res)

	// Host documents (Svelte/Vue/HTML) carry embedded <script>/<style>
	// chunks as raw_text: re-parse each with its own grammar and merge with
	// line numbers adjusted to the host file.
	if hostProf := profileFor(res.Lang); hostProf.embeddedSections != nil {
		for _, sec := range hostProf.embeddedSections(root, lang, src) {
			secLang := sectionGrammar(sec.lang)
			if secLang == nil {
				continue
			}
			secRes := &FileResult{Lang: sec.lang}
			secParser := e.parserFor(sec.lang, secLang)
			secTree, err := secParser.Parse(sec.src)
			if err != nil || secTree == nil {
				continue
			}
			walkTree(profileFor(sec.lang), secTree.RootNode(), secLang, sec.src, secRes)
			if secTree.RootNode().HasErrorOrMissing() {
				res.Err = true
			}
			lineOff := lineOffsetAt(src, sec.start)
			for i := range secRes.Symbols {
				secRes.Symbols[i].Line += lineOff
				secRes.Symbols[i].StartByte += sec.start
				secRes.Symbols[i].EndByte += sec.start
				secRes.Symbols[i].Lang = sec.lang
			}
			for i := range secRes.Edges {
				secRes.Edges[i].Line += lineOff
			}
			res.Symbols = append(res.Symbols, secRes.Symbols...)
			res.Edges = append(res.Edges, secRes.Edges...)
		}
	}
	return res, nil
}

// lineOffsetAt counts newlines before byte offset at (0-based line + 1 =
// 1-based line of at).
func lineOffsetAt(src []byte, at int) int {
	n := 0
	for i := 0; i < at && i < len(src); i++ {
		if src[i] == '\n' {
			n++
		}
	}
	return n
}

// walkTree is the language-neutral extraction pass shared by the host file
// and every embedded section.
func walkTree(prof *langProfile, root *gts.Node, lang *gts.Language, src []byte, res *FileResult) {
	// Module-scope variable bindings (JS/TS top-level `const u = new Widget()`)
	// are visible inside every function, matching the languages' semantics.
	fileVars := prof.collectVarBindings(root, lang, src)
	if len(fileVars) == 0 {
		fileVars = nil
	}
	type frame struct {
		node     *gts.Node
		caller   string      // enclosing definition (qualified when nested in a type)
		typeName string      // enclosing type name, if any
		vars     varBindings // local variable -> class bindings, offset-indexed
	}
	stack := []frame{{root, "", "", fileVars}}
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
		kind, isDef := prof.defs[nt]
		if !isDef && prof.conditionalDefs != nil && n != root {
			if cf, ok := prof.conditionalDefs[nt]; ok {
				if k, emit := cf(n, lang); emit {
					kind, isDef = k, true
				}
			}
		}
		if isDef && n != root {
			// Function/method bodies get a fresh variable-type scope: declared
			// parameter types plus local `var = new Klass()` bindings, recorded
			// with byte offsets so each call resolves the binding that precedes
			// it in source order. Languages whose type scope lives outside the
			// AST nesting (Go receivers) supply it via the methodScope hook.
			if kind == "func" || kind == "method" {
				vars = prof.collectVarBindings(n, lang, src)
				if prof.methodScope != nil {
					if recvType, recvVars := prof.methodScope(n, lang, src); recvType != "" {
						typeName = recvType
						for v, class := range recvVars {
							vars[v] = append(vars[v], varBinding{at: 0, class: class})
						}
					}
				}
				for v, bs := range fileVars { // module scope overlays function scope
					if _, shadowed := vars[v]; !shadowed {
						vars[v] = bs
					}
				}
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
				if prof.containerKinds[kind] {
					typeName = name // nested defs now qualify against this type
				}
			}
		} else if prof.calls[nt] {
			if callee, ok := calleeName(prof, n, lang, src, typeName, vars); ok && callee != "" {
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

// calleeName extracts the callee of a call node via the language profile:
// self-receivers (this/$this/self/static) and locally-typed variables
// ($obj = new User, typed parameters) qualify with their class; the
// profile's qualifyCallee hook covers language idioms beyond that (PHP
// static Class::method calls, SomeClass::class literals).
func calleeName(p *langProfile, n *gts.Node, lang *gts.Language, src []byte, typeName string, vars varBindings) (string, bool) {
	f := n
	if fn := n.ChildByFieldName("function", lang); fn != nil {
		f = fn
	}
	var last, receiverText string
	found := false
	var scanParts func(c *gts.Node)
	scanParts = func(c *gts.Node) {
		if c == nil {
			return
		}
		ct := c.Type(lang)
		if ct == "arguments" {
			return
		}
		// Member accesses wrap (object, method) in one node (python `attribute`,
		// js `member_expression`): unwrap so the object becomes the receiver.
		if unwrapReceivers[ct] {
			for j := 0; j < c.NamedChildCount(); j++ {
				scanParts(c.NamedChild(j))
			}
			return
		}
		if receiverText == "" {
			receiverText = strings.TrimSpace(string(src[c.StartByte():c.EndByte()]))
		}
		if identifierTypes[ct] {
			last = string(src[c.StartByte():c.EndByte()])
			found = true
		}
	}
	for i := 0; i < f.NamedChildCount(); i++ {
		scanParts(f.NamedChild(i))
	}
	if !found {
		if f != n && identifierTypes[f.Type(lang)] {
			return string(src[f.StartByte():f.EndByte()]), true
		}
		return "", false
	}
	// Implicit dispatch on the current instance: qualify with the type.
	if p.selfReceivers[strings.TrimPrefix(receiverText, "$")] {
		if typeName != "" {
			return typeName + "::" + last, true
		}
		return last, true
	}
	// Locally-typed variable: binding is syntactic (assignment or signature).
	if vars != nil && receiverText != "" {
		if m := varNameRe.FindStringSubmatch(receiverText); m != nil {
			if class, bound := vars.resolve(m[1], int(n.StartByte())); bound {
				return class + "::" + last, true
			}
		}
	}
	// Language-specific idioms (PHP ::class literals, static Klass::method).
	if p.qualifyCallee != nil {
		if q, ok := p.qualifyCallee(n, lang, src, receiverText, last); ok {
			return q, true
		}
	}
	return last, true
}

// unwrapReceivers are member-access wrapper nodes whose children are the
// (object, method) pair.
var unwrapReceivers = map[string]bool{
	"attribute":         true, // python
	"member_expression": true, // js/ts
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
