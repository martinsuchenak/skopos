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
			mcplib.String("branch", "Branch name to filter branch-scoped entries"),
			mcplib.String("session_id", "Session ID to include session-scoped entries"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			bundle, err := service.Bundle(ctx,
				req.StringOr("branch", ""),
				req.StringOr("session_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(bundle), nil
		},
	)
}
