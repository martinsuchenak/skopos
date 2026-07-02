package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterBlackboardTool(registerBlackboardDeleteTool)
}

func registerBlackboardDeleteTool(server *mcplib.Server, service *blackboard.Service) {
	server.RegisterTool(
		mcplib.NewTool("blackboard_delete", "Permanently delete a blackboard entry by ID",
			mcplib.String("id", "Required. ID of the entry to delete", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			id := req.StringOr("id", "")
			if err := service.Delete(ctx, id); err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"id": id, "deleted": "true"}), nil
		},
	)
}
