package mcp

import (
	"context"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterCodeIndexTool(registerCodeIndexTools)
}

const codeIndexDesc = "Use when the question NAMES an identifier (symbol, function, class). For role/description questions with no name, use code_find. For literal strings or config values, grep is fine. workspace_id defaults to the key's sole workspace; branch optional (unindexed branches fall back to the default, labeled in the response)."

func registerCodeIndexTools(server *mcplib.Server, svc *codeindex.Service) {
	server.RegisterTool(
		mcplib.NewTool("code_search", "Full-text search for symbols by name or signature (camelCase is split: 'loadconfig' matches LoadConfig). "+codeIndexDesc,
			wsParam(),
			mcplib.String("q", "Search query (prefix match)", mcplib.Required()),
			mcplib.String("path", "Optional path prefix filter (e.g. app/Services)"),
			mcplib.String("branch", "Branch to scope the search to"),
			mcplib.Integer("limit", "Max results (default 50)"),
			mcplib.Boolean("semantic", "Fuse full-text with semantic vector search (when the server has embeddings configured)"),
			mcplib.Boolean("detail", "Include doc comments, modifiers, and attributes (default false — compact: name, kind, file:line, signature)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			branch := req.StringOr("branch", "")
			q := req.StringOr("q", "")
			path := req.StringOr("path", "")
			if semantic, err := req.Bool("semantic"); err == nil && semantic && semanticSearcher != nil {
				res, err := svc.SemanticSearch(ctx, ws, branch, q, path, limit, semanticSearcher)
				if err != nil {
					return nil, toolError(err)
				}
				detail := false
				if v, derr := req.Bool("detail"); derr == nil {
					detail = v
				}
				if !detail {
					return mcplib.NewToolResponseJSON(terseSearch(res)), nil
				}
				return mcplib.NewToolResponseJSON(res), nil
			}
			res, err := svc.Search(ctx, ws, branch, q, path, limit)
			if err != nil {
				return nil, toolError(err)
			}
			detail := false
			if v, derr := req.Bool("detail"); derr == nil {
				detail = v
			}
			if !detail {
				return mcplib.NewToolResponseJSON(terseSearch(res)), nil
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_symbol", "Find definitions of a symbol by exact name (file:line + signature; detail=true adds the doc comment). "+codeIndexDesc,
			wsParam(),
			mcplib.String("name", "Exact symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Boolean("detail", "Include doc comments, modifiers, and attributes (default false — compact: name, kind, file:line, signature)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			res, err := svc.Symbol(ctx, ws, req.StringOr("branch", ""), req.StringOr("name", ""))
			// projection applied after err check below
			if err != nil {
				return nil, toolError(err)
			}
			detail := false
			if v, derr := req.Bool("detail"); derr == nil {
				detail = v
			}
			if !detail {
				return mcplib.NewToolResponseJSON(terseSearch(res)), nil
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_outline", "List a file's definitions in source order — cheaper than reading the file. "+codeIndexDesc,
			wsParam(),
			mcplib.String("path", "File path relative to the repo root", mcplib.Required()),
			mcplib.String("branch", "Branch"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			res, err := svc.Outline(ctx, ws, req.StringOr("branch", ""), req.StringOr("path", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_callers", "Who calls this name? Returns caller symbol, file, and line. Name-based heuristics. "+codeIndexDesc,
			wsParam(),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("limit", "Max results"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.CallersOfKinds(ctx, ws, req.StringOr("branch", ""), req.StringOr("name", ""), req.StringOr("path", ""), limit, parseKinds(req.StringOr("kinds", "")))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_callees", "What does this symbol call? Returns callee name, file, and line. "+codeIndexDesc,
			wsParam(),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("limit", "Max results"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.Callees(ctx, ws, req.StringOr("branch", ""), req.StringOr("name", ""), req.StringOr("path", ""), limit)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_impact", "What is transitively affected by changing this symbol? Returns callers by BFS depth (default 3, max 10). "+codeIndexDesc,
			wsParam(),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("depth", "Max BFS depth (default 3)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			depth := 0
			if n, err := req.Int("depth"); err == nil {
				depth = n
			}
			res, err := svc.Impact(ctx, ws, req.StringOr("branch", ""), req.StringOr("name", ""), depth)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_dependencies", "List each file's imports (module dependency graph) on a branch. "+codeIndexDesc,
			wsParam(),
			mcplib.String("branch", "Branch"),
			mcplib.String("path", "Optional path prefix filter"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			res, err := svc.Dependencies(ctx, ws, req.StringOr("branch", ""), req.StringOr("path", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_index_status", "Which branches of a workspace are indexed, at which git HEAD, and how fresh.",
			wsParam(),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			res, err := svc.Status(ctx, ws)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)
}

func init() {
	RegisterCodeIndexTool(registerCodeAnalysisTools)
}

func registerCodeAnalysisTools(server *mcplib.Server, svc *codeindex.Service) {
	server.RegisterTool(
		mcplib.NewTool("code_dead", "List symbols with no incoming call references (dead-code candidates; dynamic dispatch can hide usage — verify before deleting). "+codeIndexDesc,
			wsParam(),
			mcplib.String("branch", "Branch"),
			mcplib.String("path", "Optional path prefix filter"),
			mcplib.Integer("limit", "Max results (default 100)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.Dead(ctx, ws, req.StringOr("branch", ""), req.StringOr("path", ""), limit)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_cycles", "Find cycles in the call graph (up to length 6). "+codeIndexDesc,
			wsParam(),
			mcplib.String("branch", "Branch"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			res, err := svc.Cycles(ctx, ws, req.StringOr("branch", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_call_tree", "Expand what a symbol calls, recursively (tree, depth default 3). "+codeIndexDesc,
			wsParam(),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("depth", "Max depth (default 3, max 10)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			depth := 0
			if n, err := req.Int("depth"); err == nil {
				depth = n
			}
			res, err := svc.CallTree(ctx, ws, req.StringOr("branch", ""), req.StringOr("name", ""), depth)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_branch_diff", "Compare a feature branch's indexed symbols against the default branch — merge-prep intelligence (what changed, what the other side added). "+codeIndexDesc,
			wsParam(),
			mcplib.String("branch", "Feature branch to compare", mcplib.Required()),
			mcplib.String("base", "Explicit diff base (default: the workspace's default branch)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			res, err := svc.BranchDiff(ctx, ws, req.StringOr("branch", ""), req.StringOr("base", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)
}

// parseKinds splits a comma-separated edge-kind filter; empty means no
// filter (inclusive — call sites plus type references/definitions).
func parseKinds(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if k := strings.TrimSpace(part); k != "" {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// terseSearch strips doc/modifiers/attrs from hits — measured token
// economics showed chunky results are re-serialized across every subsequent
// turn; detail is one re-query with detail=true away.
func terseSearch(res *codeindex.SearchResults) *codeindex.SearchResults {
	out := &codeindex.SearchResults{Branch: res.Branch, Fallback: res.Fallback, Note: res.Note, Semantic: res.Semantic, Hits: make([]codeindex.SymbolHit, 0, len(res.Hits))}
	for _, h := range res.Hits {
		h.Doc = ""
		h.Modifiers = nil
		h.Attrs = nil
		out.Hits = append(out.Hits, h)
	}
	return out
}
