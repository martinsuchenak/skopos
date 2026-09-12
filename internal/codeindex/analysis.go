package codeindex

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// DeadResult reports symbols with no incoming call references on a branch.
// Heuristic: name-based edges miss dynamic dispatch (interfaces, callbacks,
// reflection) — treat as candidates, not verdicts.
type DeadResult struct {
	Branch   string      `json:"branch"`
	Fallback bool        `json:"fallback,omitempty"`
	Note     string      `json:"note,omitempty"`
	Notes    []string    `json:"notes,omitempty"`
	Symbols  []SymbolHit `json:"symbols"`
}

var deadExcludedPrefixes = []string{"main", "init", "Test", "test_", "__", "new_", "New"}

// Dead lists unreferenced definitions (no call edges target them).
func (s *Service) Dead(ctx context.Context, workspace, branch string, limit int) (*DeadResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT s.name, s.qual_name, s.kind, bf.path, s.line, s.signature, s.lang
		FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
		WHERE NOT EXISTS (
			SELECT 1 FROM edges e
			JOIN branch_files bf2 ON bf2.hash = e.hash AND bf2.branch = ?
			WHERE e.callee = s.name OR e.callee = s.qual_name
		)
		ORDER BY bf.path, s.line
		LIMIT ?`, resolved, resolved, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &DeadResult{Branch: label, Fallback: fallback}
	if fallback {
		out.Note = label
	}
	for rows.Next() {
		var h SymbolHit
		if err := rows.Scan(&h.Name, &h.Qualified, &h.Kind, &h.Path, &h.Line, &h.Signature, &h.Lang); err != nil {
			return nil, err
		}
		if isDeadExcluded(displayOf(h), h.Kind) {
			continue
		}
		out.Symbols = append(out.Symbols, h)
	}
	out.Notes = []string{"Heuristic: dynamic dispatch (interfaces, reflection, callbacks) can hide real usage — verify before deleting."}
	return out, rows.Err()
}

// displayOf returns the qualified form for exclusion checks and display.
func displayOf(h SymbolHit) string {
	if h.Qualified != "" {
		return h.Qualified
	}
	return h.Name
}

func isDeadExcluded(name, kind string) bool {
	// Check the short name (after Class:: qualification).
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	for _, p := range deadExcludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Cycle is one call-graph cycle (names in call order, rotating canonically),
// with each node's definition location when resolvable.
type Cycle struct {
	Names     []string          `json:"names"`
	Locations map[string]string `json:"locations,omitempty"` // name -> "path:line"
}

// CyclesResult reports call cycles found on a branch.
type CyclesResult struct {
	Branch   string  `json:"branch"`
	Fallback bool    `json:"fallback,omitempty"`
	Note     string  `json:"note,omitempty"`
	Cycles   []Cycle `json:"cycles"`
}

// Cycles finds call cycles (Johnson-style DFS with rotation dedup, capped).
func (s *Service) Cycles(ctx context.Context, workspace, branch string) (*CyclesResult, error) {
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	adj, err := s.branchGraph(workspace, resolved)
	if err != nil {
		return nil, err
	}
	out := &CyclesResult{Branch: label, Fallback: fallback}
	if fallback {
		out.Note = label
	}

	const maxLen = 6
	seen := map[string]bool{}
	for start := range adj {
		if len(out.Cycles) >= 20 {
			break
		}
		// DFS from start, only through nodes >= start (canonical rotation)
		type frame struct {
			node string
			path []string
		}
		stack := []frame{{start, []string{start}}}
		for len(stack) > 0 && len(out.Cycles) < 20 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, next := range adj[f.node] {
				if next < start {
					continue // canonical rotation: smallest node first
				}
				if next == start {
					if len(f.path) >= 2 {
						key := strings.Join(f.path, "\x00")
						if !seen[key] {
							seen[key] = true
							out.Cycles = append(out.Cycles, Cycle{Names: append([]string(nil), f.path...)})
						}
					}
					continue
				}
				if len(f.path) >= maxLen {
					continue
				}
				// skip repeats within the path
				dup := false
				for _, p := range f.path {
					if p == next {
						dup = true
						break
					}
				}
				if dup {
					continue
				}
				stack = append(stack, frame{next, append(append([]string(nil), f.path...), next)})
			}
		}
	}

	// Attach definition locations to cycle nodes (agents open files, not names).
	if len(out.Cycles) > 0 {
		var names []string
		for _, c := range out.Cycles {
			names = append(names, c.Names...)
		}
		if locs, err := s.definitionLocations(workspace, resolved, names); err == nil {
			for i := range out.Cycles {
				out.Cycles[i].Locations = map[string]string{}
				for _, n := range out.Cycles[i].Names {
					if loc, ok := locs[n]; ok {
						out.Cycles[i].Locations[n] = fmt.Sprintf("%s:%d", loc.path, loc.line)
					}
				}
			}
		}
	}
	return out, nil
}

// uniqueNameResolver returns a function mapping a bare method name to its
// fully-qualified form when exactly one symbol with that short name exists
// on the branch (and no top-level symbol shadows it); otherwise the name
// passes through unchanged.
func (s *Service) uniqueNameResolver(workspace, branch string) func(string) string {
	db, err := s.store.DB(workspace)
	if err != nil {
		return func(n string) string { return n }
	}
	rows, err := db.Query(`
		SELECT s.name,
		       COUNT(DISTINCT COALESCE(NULLIF(s.qual_name,''), s.name)) AS variants,
		       MIN(COALESCE(NULLIF(s.qual_name,''), s.name))             AS pick,
		       SUM(CASE WHEN s.qual_name = '' THEN 1 ELSE 0 END)         AS toplevel
		FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
		GROUP BY s.name`, branch)
	if err != nil {
		return func(n string) string { return n }
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var name, pick string
		var variants, toplevel int
		if err := rows.Scan(&name, &variants, &pick, &toplevel); err != nil {
			return func(n string) string { return n }
		}
		if variants == 1 && toplevel == 0 && pick != name {
			m[name] = pick
		}
	}
	return func(n string) string {
		if strings.Contains(n, "::") {
			return n
		}
		if q, ok := m[n]; ok {
			return q
		}
		return n
	}
}

// branchGraph loads the distinct caller->callee adjacency for a branch.
func (s *Service) branchGraph(workspace, branch string) (map[string][]string, error) {
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT DISTINCT e.caller, e.callee
		FROM edges e
		JOIN branch_files bf ON bf.hash = e.hash AND bf.branch = ?
		WHERE e.caller != '' AND e.caller != e.callee`, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	adj := map[string]map[string]bool{}
	for rows.Next() {
		var caller, callee string
		if err := rows.Scan(&caller, &callee); err != nil {
			return nil, err
		}
		if adj[caller] == nil {
			adj[caller] = map[string]bool{}
		}
		adj[caller][callee] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(adj))
	for caller, set := range adj {
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names) // deterministic DFS order
		out[caller] = names
	}
	return out, nil
}

// CallTreeNode is one node of a recursive callee tree, with the node's
// definition location when resolvable (top-level: names alone are not
// enough to open a file).
type CallTreeNode struct {
	Name     string         `json:"name"`
	Path     string         `json:"path,omitempty"`
	Line     int            `json:"line,omitempty"`
	Children []CallTreeNode `json:"children,omitempty"`
}

// CallTree expands the callee tree from a symbol, bounded by depth and node
// count. Repeated names expand only once (first occurrence wins) to keep the
// tree finite on cycles.
func (s *Service) CallTree(ctx context.Context, workspace, branch, name string, depth int) (*CallTreeResult, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if depth <= 0 || depth > 10 {
		depth = 3
	}
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	adj, err := s.branchGraph(workspace, resolved)
	if err != nil {
		return nil, err
	}
	const maxNodes = 400
	budget := maxNodes
	seen := map[string]bool{}

	// Bare callees (calls on non-$this receivers) merge every same-named
	// method into one node. When exactly one symbol on the branch carries
	// that short name, resolve the display name to its FQN — ambiguous or
	// unknown names stay bare rather than guessing.
	resolve := s.uniqueNameResolver(workspace, resolved)

	var expand func(n string, d int) CallTreeNode
	expand = func(n string, d int) CallTreeNode {
		node := CallTreeNode{Name: n}
		if d >= depth || budget <= 0 {
			return node
		}
		for _, callee := range adj[n] {
			if budget <= 0 {
				break
			}
			if seen[callee] {
				continue // already expanded somewhere: keep the tree acyclic
			}
			seen[callee] = true
			budget--
			node.Children = append(node.Children, expand(resolve(callee), d+1))
		}
		return node
	}
	seen[name] = true
	tree := expand(name, 0)

	// Attach definition locations to every node (agents open files, not names).
	var collect func(n CallTreeNode, out *[]string)
	collect = func(n CallTreeNode, out *[]string) {
		*out = append(*out, n.Name)
		for _, c := range n.Children {
			collect(c, out)
		}
	}
	var names []string
	collect(tree, &names)
	if locs, err := s.definitionLocations(workspace, resolved, names); err == nil {
		var attach func(n *CallTreeNode)
		attach = func(n *CallTreeNode) {
			if loc, ok := locs[n.Name]; ok {
				n.Path, n.Line = loc.path, loc.line
			}
			for i := range n.Children {
				attach(&n.Children[i])
			}
		}
		attach(&tree)
	}
	return &CallTreeResult{Branch: label, Fallback: fallback, Tree: tree}, nil
}

// CallTreeResult wraps a call tree with branch labeling.
type CallTreeResult struct {
	Branch   string       `json:"branch"`
	Fallback bool         `json:"fallback,omitempty"`
	Note     string       `json:"note,omitempty"`
	Tree     CallTreeNode `json:"tree"`
}

// BranchDiffResult compares a branch's indexed state against the default
// branch: files added/removed/changed and the symbol-level deltas.
type BranchDiffResult struct {
	Branch  string        `json:"branch"`
	Base    string        `json:"base"`
	Files   []FileChange  `json:"files"`
	Symbols []SymbolDelta `json:"symbols"`
}

// FileChange is one changed file between base and branch.
type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added | removed | changed
}

// SymbolDelta is a symbol present on only one side of the diff, or renamed
// (paired by same file + kind + declaration with the name masked out).
type SymbolDelta struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Change   string `json:"change"`              // added | removed | renamed
	PrevName string `json:"prev_name,omitempty"` // previous name when change=renamed

	Signature string `json:"-"` // declaration text, used for rename pairing
}

// BranchDiff compares a branch against the default branch's index.
func (s *Service) BranchDiff(ctx context.Context, workspace, branch, base string) (*BranchDiffResult, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("%w: branch is required", ErrInvalidInput)
	}
	// Diffing a branch with no index state silently compares against an
	// empty tree (everything shows as removed) — reject with the indexed
	// branches so the next command is obvious.
	indexed, err := s.store.Status(workspace)
	if err != nil {
		return nil, err
	}
	if len(indexed) == 0 {
		return nil, fmt.Errorf("%w: no branches are indexed for this workspace yet — index one first (skopos index build / push)", ErrInvalidInput)
	}
	known := make([]string, 0, len(indexed))
	for _, st := range indexed {
		known = append(known, st.Branch)
	}
	if ok, _ := s.store.BranchIndexed(workspace, branch); !ok {
		return nil, fmt.Errorf("%w: branch %q is not indexed (indexed: %s) — index it first, then diff it against the default", ErrInvalidInput, branch, strings.Join(known, ", "))
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = s.store.DefaultBranch(workspace)
	} else if ok, _ := s.store.BranchIndexed(workspace, base); !ok {
		return nil, fmt.Errorf("%w: base branch %q is not indexed (indexed: %s)", ErrInvalidInput, base, strings.Join(known, ", "))
	}
	if branch == base {
		return nil, fmt.Errorf("%w: branch %s is the diff base — diff a feature branch against it (skopos branch-diff <feature> or --base <other>)", ErrInvalidInput, branch)
	}
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	files := func(br string) (map[string]string, error) {
		rows, err := db.Query(`SELECT path, hash FROM branch_files WHERE branch = ?`, br)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var p, h string
			if err := rows.Scan(&p, &h); err != nil {
				return nil, err
			}
			m[p] = h
		}
		return m, rows.Err()
	}
	baseFiles, err := files(base)
	if err != nil {
		return nil, err
	}
	branchFiles, err := files(branch)
	if err != nil {
		return nil, err
	}
	symbolsOf := func(hash string) (map[string]SymbolHit, error) {
		rows, err := db.Query(`
			SELECT s.name, s.kind, s.line, s.signature FROM symbols s WHERE s.hash = ?`, hash)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]SymbolHit{}
		for rows.Next() {
			var h SymbolHit
			if err := rows.Scan(&h.Name, &h.Kind, &h.Line, &h.Signature); err != nil {
				return nil, err
			}
			m[h.Name+"\x00"+h.Kind] = h
		}
		return m, rows.Err()
	}

	out := &BranchDiffResult{Branch: branch, Base: base}
	for path, hash := range branchFiles {
		baseHash, ok := baseFiles[path]
		switch {
		case !ok:
			out.Files = append(out.Files, FileChange{Path: path, Change: "added"})
		case baseHash != hash:
			out.Files = append(out.Files, FileChange{Path: path, Change: "changed"})
		}
	}
	for path := range baseFiles {
		if _, ok := branchFiles[path]; !ok {
			out.Files = append(out.Files, FileChange{Path: path, Change: "removed"})
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })

	for _, fc := range out.Files {
		if fc.Change == "removed" {
			continue // symbol removals for deleted files are noisy; file-level note suffices
		}
		branchSyms, err := symbolsOf(branchFiles[fc.Path])
		if err != nil {
			return nil, err
		}
		var baseSyms map[string]SymbolHit
		if baseHash, ok := baseFiles[fc.Path]; ok {
			if baseSyms, err = symbolsOf(baseHash); err != nil {
				return nil, err
			}
		}
		for key, sym := range branchSyms {
			if _, ok := baseSyms[key]; !ok {
				out.Symbols = append(out.Symbols, SymbolDelta{Name: sym.Name, Kind: sym.Kind, Path: fc.Path, Change: "added", Signature: sym.Signature})
			}
		}
		for key, sym := range baseSyms {
			if _, ok := branchSyms[key]; !ok {
				out.Symbols = append(out.Symbols, SymbolDelta{Name: sym.Name, Kind: sym.Kind, Path: fc.Path, Change: "removed", Signature: sym.Signature})
			}
		}
	}
	pairRenames(out)
	sort.Slice(out.Symbols, func(i, j int) bool {
		if out.Symbols[i].Path != out.Symbols[j].Path {
			return out.Symbols[i].Path < out.Symbols[j].Path
		}
		return out.Symbols[i].Name < out.Symbols[j].Name
	})
	return out, nil
}

// pairRenames matches removed+added symbol pairs in the same file with the
// same kind whose declarations agree once the name is masked out — the
// `method old() {}` -> `method renamed() {}` shape — and rewrites them as a
// single renamed delta (git's rename-detection analogue for symbols).
func pairRenames(out *BranchDiffResult) {
	removed := map[int]bool{} // indexes into out.Symbols
	added := map[int]bool{}
	for i, d := range out.Symbols {
		if d.Change == "removed" && d.Signature != "" {
			removed[i] = true
		}
		if d.Change == "added" && d.Signature != "" {
			added[i] = true
		}
	}
	// Deterministic order: best name-similarity wins when several
	// same-signature candidates exist (an empty-bodied `func x() {}`
	// matches everything after masking).
	for ri := range removed {
		r := out.Symbols[ri]
		best, bestScore := -1, -1
		for ai := range added {
			a := out.Symbols[ai]
			if r.Path != a.Path || r.Kind != a.Kind {
				continue
			}
			if maskName(r.Signature, r.Name) != maskName(a.Signature, a.Name) {
				continue
			}
			score := nameSimilarity(r.Name, a.Name)
			if score > bestScore {
				best, bestScore = ai, score
			}
		}
		if best >= 0 {
			a := out.Symbols[best]
			out.Symbols[best] = SymbolDelta{
				Name: a.Name, Kind: a.Kind, Path: a.Path,
				Change: "renamed", PrevName: r.Name,
			}
			out.Symbols[ri].Change = "consumed" // dropped below
			delete(added, best)
		}
	}
	kept := out.Symbols[:0]
	for _, d := range out.Symbols {
		if d.Change != "consumed" {
			kept = append(kept, d)
		}
	}
	out.Symbols = kept
}

// nameSimilarity scores how much two names share (bigram overlap), so the
// most plausible removed/added pair wins among same-signature candidates.
func nameSimilarity(a, b string) int {
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	counts := map[string]int{}
	for i := 0; i+2 <= len(a); i++ {
		counts[a[i:i+2]]++
	}
	score := 0
	for i := 0; i+2 <= len(b); i++ {
		if bg := b[i : i+2]; counts[bg] > 0 {
			counts[bg]--
			score++
		}
	}
	return score
}

// maskName replaces the first occurrence of name in sig with a placeholder so
// two declarations differing only in the symbol name compare equal.
func maskName(sig, name string) string {
	if i := strings.Index(sig, name); i >= 0 {
		return sig[:i] + "\x00" + sig[i+len(name):]
	}
	return sig
}

// firstOther picks the first branch that isn't skip, for hint messages.
func firstOther(branches []string, skip string) string {
	for _, b := range branches {
		if b != skip {
			return b
		}
	}
	return "<feature-branch>"
}
