package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterBlackboardTool(registerBlackboardReadTool)
}

func registerBlackboardReadTool(server *mcplib.Server, service *blackboard.Service) {
	server.RegisterTool(
		mcplib.NewTool("blackboard_read", "Read the Skopos blackboard Knowledge Bundle",
			wsParam(),
			mcplib.String("branch", "Branch name to filter branch-scoped entries"),
			mcplib.String("entry_type", "Filter by type: finding, decision, bug, debt, warning, context"),
			mcplib.String("author", "Filter by author agent ID"),
			mcplib.String("author_agent_id", "Alias of author (matches blackboard_write naming)"),
			mcplib.String("q", "Text search (matches title or content)"),
			mcplib.String("session_id", "Session ID to include session-scoped entries (also applies to search)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			// workspace_id is an authorization boundary, not an optional
			// filter: an omitted scope fails closed unless the key has
			// exactly one workspace (then it defaults — no discovery turn).
			workspaceID, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			entryType := req.StringOr("entry_type", "")
			author := req.StringOr("author", "")
			if author == "" {
				author = req.StringOr("author_agent_id", "")
			}
			query := req.StringOr("q", "")
			// When search filters are present, use Search; otherwise return the full Bundle.
			if entryType != "" || author != "" || query != "" {
				entries, err := service.Search(ctx, blackboard.SearchFilters{
					WorkspaceID:   workspaceID,
					BranchName:    req.StringOr("branch", ""),
					SessionID:     req.StringOr("session_id", ""),
					EntryType:     entryType,
					AuthorAgentID: author,
					Query:         query,
				})
				if err != nil {
					return nil, toolError(err)
				}
				return mcplib.NewToolResponseJSON(map[string]any{"entries": entries, "total": len(entries)}), nil
			}
			bundle, err := service.Bundle(ctx, workspaceID, req.StringOr("branch", ""), req.StringOr("session_id", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(bundle), nil
		},
	)
}
