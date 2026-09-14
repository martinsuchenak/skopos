package mcp

import (
	"context"
	"fmt"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterBlackboardTool(registerBlackboardReadTool)
}

func registerBlackboardReadTool(server *mcplib.Server, service *blackboard.Service) {
	server.RegisterTool(
		mcplib.NewTool("blackboard_read", "Read the Skopos blackboard Knowledge Bundle",
			mcplib.String("workspace_id", "Required. Workspace ID to scope entries by (derive it with `skopos workspace` or from the git remote) — unscoped reads would span every workspace on the server"),
			mcplib.String("branch", "Branch name to filter branch-scoped entries"),
			mcplib.String("entry_type", "Filter by type: finding, decision, bug, debt, warning, context"),
			mcplib.String("author", "Filter by author agent ID"),
			mcplib.String("author_agent_id", "Alias of author (matches blackboard_write naming)"),
			mcplib.String("q", "Text search (matches title or content)"),
			mcplib.String("session_id", "Session ID to include session-scoped entries (also applies to search)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			// workspace_id is an authorization boundary, not an optional
			// filter: an omitted scope must fail closed instead of silently
			// widening to every workspace's entries.
			workspaceID := req.StringOr("workspace_id", "")
			if workspaceID == "" {
				return nil, toolError(fmt.Errorf("%w: workspace_id is required — unscoped reads would return every workspace's entries; derive it with `skopos workspace` or the git remote (e.g. github.com/owner/repo)", blackboard.ErrInvalidInput))
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
