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
			mcplib.String("workspace_id", "Workspace ID to filter entries by"),
			mcplib.String("branch", "Branch name to filter branch-scoped entries"),
			mcplib.String("entry_type", "Filter by type: finding, decision, bug, debt, warning, context"),
			mcplib.String("author", "Filter by author agent ID"),
			mcplib.String("q", "Text search (matches title or content)"),
			mcplib.String("session_id", "Session ID to include session-scoped entries"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			entryType := req.StringOr("entry_type", "")
			author := req.StringOr("author", "")
			query := req.StringOr("q", "")
			// When search filters are present, use Search; otherwise return the full Bundle.
			if entryType != "" || author != "" || query != "" {
				entries, err := service.Search(ctx, blackboard.SearchFilters{
					WorkspaceID:   req.StringOr("workspace_id", ""),
					BranchName:    req.StringOr("branch", ""),
					EntryType:     entryType,
					AuthorAgentID: author,
					Query:         query,
				})
				if err != nil {
					return nil, mcplib.NewToolErrorInternal(err.Error())
				}
				return mcplib.NewToolResponseJSON(map[string]any{"entries": entries, "total": len(entries)}), nil
			}
			workspaceID := req.StringOr("workspace_id", "")
			bundle, err := service.Bundle(ctx, workspaceID, req.StringOr("branch", ""), req.StringOr("session_id", ""))
			if err != nil {
				return nil, mcplib.NewToolErrorInternal(err.Error())
			}
			return mcplib.NewToolResponseJSON(bundle), nil
		},
	)
}
