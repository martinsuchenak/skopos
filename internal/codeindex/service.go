package codeindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	store   *Store
	vectors VectorStore
}

func NewService(store *Store) *Service {
	return &Service{store: store, vectors: NewSQLiteVectorStore(store)}
}

// SetVectorStore swaps the vector backend (default: SQLite brute force;
// QdrantVectorStore for monorepo scale). Must be called before any
// embedding/search activity.
func (s *Service) SetVectorStore(vs VectorStore) {
	if vs != nil {
		s.vectors = vs
	}
}

// VectorStoreName reports the active vector backend (for status/logging).
func (s *Service) VectorStoreName() string { return s.vectors.Name() }

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
	Semantic bool        `json:"semantic,omitempty"`
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

	// Roots: the exact name plus, when the input is a bare method name, every
	// qualified match (Class::name) — each is an independent traversal node.
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	rootRows, err := db.Query(`
		SELECT DISTINCT COALESCE(NULLIF(qual_name, ''), name)
		FROM symbols
		WHERE name = ? COLLATE NOCASE OR qual_name = ? COLLATE NOCASE`, name, name)
	if err != nil {
		return nil, err
	}
	var roots []string
	for rootRows.Next() {
		var r string
		if err := rootRows.Scan(&r); err != nil {
			rootRows.Close()
			return nil, err
		}
		roots = append(roots, r)
	}
	rootRows.Close()
	// The input name itself is always a root: a symbol row carries both name
	// and qual_name, so the query above yields only the qualified form and a
	// bare-name root (whose edges store the bare callee) would be lost.
	found := false
	for _, r := range roots {
		if strings.EqualFold(r, name) {
			found = true
			break
		}
	}
	if !found {
		roots = append(roots, name)
	}

	visited := map[string]int{}
	var frontier []string
	for _, r := range roots {
		visited[r] = 0
		frontier = append(frontier, r)
	}
	var affected []ImpactNode
	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, cur := range frontier {
			edges, err := s.store.exactCallers(db, resolved, cur, 200)
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
	// Attach each affected symbol's definition location (path:line).
	if len(affected) > 0 {
		names := make([]string, len(affected))
		for i, a := range affected {
			names[i] = a.Name
		}
		locs, err := s.definitionLocations(workspace, resolved, names)
		if err == nil {
			for i := range affected {
				if loc, ok := locs[affected[i].Name]; ok {
					affected[i].Path, affected[i].Line = loc.path, loc.line
				}
			}
		}
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

type defLoc struct {
	path string
	line int
}

// definitionLocations batch-resolves name -> first definition (path, line)
// on a branch; names without a match are absent from the result.
func (s *Service) definitionLocations(workspace, branch string, names []string) (map[string]defLoc, error) {
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	out := map[string]defLoc{}
	const chunk = 100
	for start := 0; start < len(names); start += chunk {
		end := start + chunk
		if end > len(names) {
			end = len(names)
		}
		qmarks := strings.Repeat("?,", end-start)
		qmarks = strings.TrimSuffix(qmarks, ",")
		// Names appear in both IN lists; build the argument slice explicitly —
		// append(args, args[1:]...) would alias and corrupt the slice.
		queryArgs := make([]any, 0, 2*(end-start)+1)
		queryArgs = append(queryArgs, branch)
		for _, n := range names[start:end] {
			queryArgs = append(queryArgs, n)
		}
		for _, n := range names[start:end] {
			queryArgs = append(queryArgs, n)
		}
		rows, err := db.Query(`
			SELECT COALESCE(NULLIF(s.qual_name,''), s.name), bf.path, s.line
			FROM symbols s
			JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
			WHERE s.name IN (`+qmarks+`) OR s.qual_name IN (`+qmarks+`)
			GROUP BY 1`, queryArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n, p string
			var l int
			if err := rows.Scan(&n, &p, &l); err != nil {
				rows.Close()
				return nil, err
			}
			if _, seen := out[n]; !seen {
				out[n] = defLoc{p, l}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ImpactNode is one transitively-affected symbol, with where it is defined.
type ImpactNode struct {
	Name  string `json:"name"`
	Depth int    `json:"depth"`
	Path  string `json:"path,omitempty"`
	Line  int    `json:"line,omitempty"`
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

// DropWorkspace tears down a workspace's entire index: vectors (any
// backend, including external stores) and the index database itself.
func (s *Service) DropWorkspace(ctx context.Context, workspace string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("%w: workspace is required", ErrInvalidInput)
	}
	if err := s.vectors.DropWorkspace(ctx, workspace); err != nil {
		return err
	}
	return s.store.DeleteWorkspace(workspace)
}

// ---- ingest (shared by CLI and REST push) ----

// Build parses every supported file under root, returning the file results
// and the git HEAD sha of that checkout ("" when git is unavailable).
func Build(ctx context.Context, ex *parse.Extractor, root, branch string) ([]*parse.FileResult, string, error) {
	return BuildWithProgress(ctx, ex, root, branch, nil)
}

// BuildWithProgress is Build with a progress callback: report is invoked
// after each file parses, carrying files done / total found.
func BuildWithProgress(ctx context.Context, ex *parse.Extractor, root, branch string, report func(done, total int)) ([]*parse.FileResult, string, error) {
	return BuildWithCache(ctx, ex, root, branch, report, nil)
}

// BuildWithCache adds a parse cache: files whose (mtime, size, extractor)
// are unchanged skip reading/parsing entirely — the stored blob for their
// content hash is reused at commit time. Cache hits yield stub results
// (hash + language, no symbols/edges) which is all Commit needs.
func BuildWithCache(ctx context.Context, ex *parse.Extractor, root, branch string, report func(done, total int), cacher BuildCacher) ([]*parse.FileResult, string, error) {
	files, err := parse.Walk(root)
	if err != nil {
		return nil, "", err
	}
	if report != nil {
		report(0, len(files))
	}
	out := make([]*parse.FileResult, 0, len(files))
	for _, f := range files {
		var res *parse.FileResult
		if cacher != nil {
			if info, serr := os.Stat(f); serr == nil {
				if hash, ok := cacher.CacheLookup(ctx, f, info.ModTime().UnixNano(), info.Size()); ok {
					if rel, rerr := filepath.Rel(root, f); rerr == nil {
						res = &parse.FileResult{Path: rel, Hash: hash, Lang: parse.Detect(f)}
					}
				}
			}
		}
		parsed := res == nil
		if parsed {
			var perr error
			res, perr = ex.ParseFile(f)
			if perr != nil {
				continue // unreadable file: skip, keep going
			}
			// Store paths relative to the indexed root so the index is portable.
			if rel, rerr := filepath.Rel(root, res.Path); rerr == nil {
				res.Path = rel
			}
			if cacher != nil {
				if info, serr := os.Stat(f); serr == nil {
					_ = cacher.CachePut(ctx, f, info.ModTime().UnixNano(), info.Size(), res.Hash)
				}
			}
		}
		_ = parsed
		out = append(out, res)
		if report != nil {
			report(len(out), len(files))
		}
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
