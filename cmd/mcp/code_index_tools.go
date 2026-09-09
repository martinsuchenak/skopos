package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterCodeIndexTool(registerCodeIndexTools)
}

const codeIndexDesc = "Query the central code index for a workspace. Pass workspace_id (required) and branch (optional — unindexed branches fall back to the default branch, labeled in the response). Every response carries the branch/index state it answered from. Prefer these tools over grep/rg when exploring structure."

func registerCodeIndexTools(server *mcplib.Server, svc *codeindex.Service) {
	server.RegisterTool(
		mcplib.NewTool("code_search", "Full-text search for symbols by name or signature (camelCase is split: 'loadconfig' matches LoadConfig). "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID to search", mcplib.Required()),
			mcplib.String("q", "Search query (prefix match)", mcplib.Required()),
			mcplib.String("branch", "Branch to scope the search to"),
			mcplib.Integer("limit", "Max results (default 50)"),
			mcplib.Boolean("semantic", "Fuse full-text with semantic vector search (when the server has embeddings configured)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.Search(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("q", ""), limit)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_symbol", "Find definitions of a symbol by exact name (returns file:line). "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("name", "Exact symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			res, err := svc.Symbol(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("name", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_outline", "List a file's definitions in source order — cheaper than reading the file. "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("path", "File path relative to the repo root", mcplib.Required()),
			mcplib.String("branch", "Branch"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			res, err := svc.Outline(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("path", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_callers", "Who calls this name? Returns caller symbol, file, and line. Name-based heuristics. "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("limit", "Max results"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.Callers(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("name", ""), limit)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_callees", "What does this symbol call? Returns callee name, file, and line. "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("limit", "Max results"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.Callees(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("name", ""), limit)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_impact", "What is transitively affected by changing this symbol? Returns callers by BFS depth (default 3, max 10). "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("depth", "Max BFS depth (default 3)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			depth := 0
			if n, err := req.Int("depth"); err == nil {
				depth = n
			}
			res, err := svc.Impact(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("name", ""), depth)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_index_status", "Which branches of a workspace are indexed, at which git HEAD, and how fresh.",
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			res, err := svc.Status(ctx, req.StringOr("workspace_id", ""))
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
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("limit", "Max results (default 100)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			limit := 0
			if n, err := req.Int("limit"); err == nil {
				limit = n
			}
			res, err := svc.Dead(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), limit)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_cycles", "Find cycles in the call graph (up to length 6). "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("branch", "Branch"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			res, err := svc.Cycles(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_call_tree", "Expand what a symbol calls, recursively (tree, depth default 3). "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("name", "Symbol name", mcplib.Required()),
			mcplib.String("branch", "Branch"),
			mcplib.Integer("depth", "Max depth (default 3, max 10)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			depth := 0
			if n, err := req.Int("depth"); err == nil {
				depth = n
			}
			res, err := svc.CallTree(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""), req.StringOr("name", ""), depth)
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)

	server.RegisterTool(
		mcplib.NewTool("code_branch_diff", "Compare a feature branch's indexed symbols against the default branch — merge-prep intelligence (what changed, what the other side added). "+codeIndexDesc,
			mcplib.String("workspace_id", "Workspace ID", mcplib.Required()),
			mcplib.String("branch", "Feature branch to compare", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			res, err := svc.BranchDiff(ctx, req.StringOr("workspace_id", ""), req.StringOr("branch", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(res), nil
		},
	)
}
