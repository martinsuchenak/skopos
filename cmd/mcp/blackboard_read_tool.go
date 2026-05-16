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
			mcplib.String("workspace", "Workspace ID to filter entries by (alias for workspace_id)"),
			mcplib.String("branch", "Branch name to filter branch-scoped entries"),
			mcplib.String("session_id", "Session ID to include session-scoped entries"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			workspaceID := req.StringOr("workspace_id", req.StringOr("workspace", ""))
			bundle, err := service.Bundle(ctx,
				workspaceID,
				req.StringOr("branch", ""),
				req.StringOr("session_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInternal(err.Error())
			}
			return mcplib.NewToolResponseJSON(bundle), nil
		},
	)
}
