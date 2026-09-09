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
		SELECT s.name, s.kind, bf.path, s.line, s.signature, s.lang
		FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
		WHERE NOT EXISTS (
			SELECT 1 FROM edges e
			JOIN branch_files bf2 ON bf2.hash = e.hash AND bf2.branch = ?
			WHERE e.callee = s.name
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
		if err := rows.Scan(&h.Name, &h.Kind, &h.Path, &h.Line, &h.Signature, &h.Lang); err != nil {
			return nil, err
		}
		if isDeadExcluded(h.Name, h.Kind) {
			continue
		}
		out.Symbols = append(out.Symbols, h)
	}
	out.Notes = []string{"Heuristic: dynamic dispatch (interfaces, reflection, callbacks) can hide real usage — verify before deleting."}
	return out, rows.Err()
}

func isDeadExcluded(name, kind string) bool {
	for _, p := range deadExcludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Cycle is one call-graph cycle (names in call order, rotating canonically).
type Cycle struct {
	Names []string `json:"names"`
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
	return out, nil
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

// CallTreeNode is one node of a recursive callee tree.
type CallTreeNode struct {
	Name     string         `json:"name"`
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
			node.Children = append(node.Children, expand(callee, d+1))
		}
		return node
	}
	seen[name] = true
	tree := expand(name, 0)
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

// SymbolDelta is a symbol present on only one side of the diff.
type SymbolDelta struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Change string `json:"change"` // added | removed
}

// BranchDiff compares a branch against the default branch's index.
func (s *Service) BranchDiff(ctx context.Context, workspace, branch string) (*BranchDiffResult, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("%w: branch is required", ErrInvalidInput)
	}
	base := s.store.DefaultBranch(workspace)
	if branch == base {
		// Nothing to diff against itself; compare against the next best base.
		return nil, fmt.Errorf("%w: branch %s is the default branch — diff needs a feature branch", ErrInvalidInput, branch)
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
			SELECT s.name, s.kind, s.line FROM symbols s WHERE s.hash = ?`, hash)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]SymbolHit{}
		for rows.Next() {
			var h SymbolHit
			if err := rows.Scan(&h.Name, &h.Kind, &h.Line); err != nil {
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
				out.Symbols = append(out.Symbols, SymbolDelta{Name: sym.Name, Kind: sym.Kind, Path: fc.Path, Change: "added"})
			}
		}
		for key, sym := range baseSyms {
			if _, ok := branchSyms[key]; !ok {
				out.Symbols = append(out.Symbols, SymbolDelta{Name: sym.Name, Kind: sym.Kind, Path: fc.Path, Change: "removed"})
			}
		}
	}
	sort.Slice(out.Symbols, func(i, j int) bool {
		if out.Symbols[i].Path != out.Symbols[j].Path {
			return out.Symbols[i].Path < out.Symbols[j].Path
		}
		return out.Symbols[i].Name < out.Symbols[j].Name
	})
	return out, nil
}
