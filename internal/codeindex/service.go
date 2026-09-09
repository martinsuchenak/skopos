package codeindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

func marshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ErrInvalidInput marks client mistakes (mirrors the domain packages).
var ErrInvalidInput = fmt.Errorf("invalid code index input")

// Service adds branch-fallback and graph semantics on top of Store.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

func (s *Service) Store() *Store { return s.store }

// resolveBranch applies fallback: an unindexed branch answers from the
// default branch, labeled as a fallback.
func (s *Service) resolveBranch(workspace, branch string) (resolved, label string, fallback bool, err error) {
	if branch == "" {
		branch = s.store.DefaultBranch(workspace)
	}
	indexed, err := s.store.BranchIndexed(workspace, branch)
	if err != nil {
		return "", "", false, err
	}
	if indexed {
		return branch, branch, false, nil
	}
	def := s.store.DefaultBranch(workspace)
	return def, fmt.Sprintf("%s (branch %q not indexed — showing %s)", def, branch, def), true, nil
}

// SearchResults carries hits plus which index state answered.
type SearchResults struct {
	Branch   string      `json:"branch"`
	Fallback bool        `json:"fallback,omitempty"`
	Note     string      `json:"note,omitempty"`
	Hits     []SymbolHit `json:"hits"`
}

func (s *Service) Search(ctx context.Context, workspace, branch, query string, limit int) (*SearchResults, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query is required", ErrInvalidInput)
	}
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	hits, err := s.store.Search(workspace, resolved, query, limit)
	if err != nil {
		return nil, err
	}
	out := &SearchResults{Branch: label, Fallback: fallback, Hits: hits}
	if fallback {
		out.Note = label
	}
	return out, nil
}

func (s *Service) Symbol(ctx context.Context, workspace, branch, name string) (*SearchResults, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	hits, err := s.store.Symbol(workspace, resolved, name, 0)
	if err != nil {
		return nil, err
	}
	out := &SearchResults{Branch: label, Fallback: fallback, Hits: hits}
	if fallback {
		out.Note = label
	}
	return out, nil
}

func (s *Service) Outline(ctx context.Context, workspace, branch, path string) (*SearchResults, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: path is required", ErrInvalidInput)
	}
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	hits, err := s.store.Outline(workspace, resolved, strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, err
	}
	return &SearchResults{Branch: label, Fallback: fallback, Hits: hits}, nil
}

// GraphResults carries edges plus index-state labeling.
type GraphResults struct {
	Branch   string    `json:"branch"`
	Fallback bool      `json:"fallback,omitempty"`
	Note     string    `json:"note,omitempty"`
	Edges    []EdgeHit `json:"edges"`
}

func (s *Service) Callers(ctx context.Context, workspace, branch, name string, limit int) (*GraphResults, error) {
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	edges, err := s.store.Callers(workspace, resolved, name, limit)
	if err != nil {
		return nil, err
	}
	out := &GraphResults{Branch: label, Fallback: fallback, Edges: edges}
	if fallback {
		out.Note = label
	}
	return out, nil
}

func (s *Service) Callees(ctx context.Context, workspace, branch, name string, limit int) (*GraphResults, error) {
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}
	edges, err := s.store.Callees(workspace, resolved, name, limit)
	if err != nil {
		return nil, err
	}
	out := &GraphResults{Branch: label, Fallback: fallback, Edges: edges}
	if fallback {
		out.Note = label
	}
	return out, nil
}

// Impact returns the transitive set of symbols that (transitively) call the
// given name — everything potentially affected by changing it. BFS over the
// caller edges, bounded by maxDepth.
func (s *Service) Impact(ctx context.Context, workspace, branch, name string, maxDepth int) (*ImpactResults, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if maxDepth <= 0 || maxDepth > 10 {
		maxDepth = 3
	}
	resolved, label, fallback, err := s.resolveBranch(workspace, branch)
	if err != nil {
		return nil, err
	}

	visited := map[string]int{name: 0}
	frontier := []string{name}
	var affected []ImpactNode
	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, cur := range frontier {
			edges, err := s.store.Callers(workspace, resolved, cur, 200)
			if err != nil {
				return nil, err
			}
			for _, e := range edges {
				caller := e.Caller
				if caller == "" || caller == cur {
					continue // file-scope call or self-loop
				}
				if _, seen := visited[caller]; !seen {
					visited[caller] = depth
					affected = append(affected, ImpactNode{Name: caller, Depth: depth})
					next = append(next, caller)
				}
			}
		}
		frontier = next
	}
	sort.Slice(affected, func(i, j int) bool {
		if affected[i].Depth != affected[j].Depth {
			return affected[i].Depth < affected[j].Depth
		}
		return affected[i].Name < affected[j].Name
	})
	return &ImpactResults{
		Branch: label, Fallback: fallback, Root: name,
		Affected: affected,
	}, nil
}

// ImpactNode is one transitively-affected symbol.
type ImpactNode struct {
	Name  string `json:"name"`
	Depth int    `json:"depth"`
}

// ImpactResults is the transitive-caller set for a symbol.
type ImpactResults struct {
	Branch   string       `json:"branch"`
	Fallback bool         `json:"fallback,omitempty"`
	Root     string       `json:"root"`
	Affected []ImpactNode `json:"affected"`
}

// Status lists indexed branches.
func (s *Service) Status(ctx context.Context, workspace string) ([]BranchStatus, error) {
	return s.store.Status(workspace)
}

// DropBranch removes a branch's index state.
func (s *Service) DropBranch(ctx context.Context, workspace, branch string) error {
	if branch == "" {
		return fmt.Errorf("%w: branch is required", ErrInvalidInput)
	}
	return s.store.DropBranch(workspace, branch)
}

// ---- ingest (shared by CLI and REST push) ----

// Build parses every supported file under root, returning the file results
// and the git HEAD sha of that checkout ("" when git is unavailable).
func Build(ctx context.Context, ex *parse.Extractor, root, branch string) ([]*parse.FileResult, string, error) {
	files, err := parse.Walk(root)
	if err != nil {
		return nil, "", err
	}
	out := make([]*parse.FileResult, 0, len(files))
	for _, f := range files {
		res, err := ex.ParseFile(f)
		if err != nil {
			continue // unreadable file: skip, keep going
		}
		// Store paths relative to the indexed root so the index is portable.
		if rel, err := filepath.Rel(root, res.Path); err == nil {
			res.Path = rel
		}
		out = append(out, res)
	}
	head := gitHead(root)
	return out, head, nil
}

// CommitLocal materializes results into a workspace's index DB directly
// (used for local builds; the remote path negotiates via Manifest/AddBlob).
func CommitLocal(store *Store, workspace, branch, source string, results []*parse.FileResult, head string) error {
	entries := make([]FileEntry, 0, len(results))
	for _, res := range results {
		if err := store.AddBlob(workspace, res); err != nil {
			return err
		}
		entries = append(entries, FileEntry{Path: strings.TrimPrefix(res.Path, "/"), Hash: res.Hash})
	}
	return store.Commit(workspace, branch, head, source, entries)
}

func gitHead(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(bytes.TrimSpace(out)))
}
