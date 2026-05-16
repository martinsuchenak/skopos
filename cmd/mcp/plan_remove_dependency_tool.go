package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanRemoveDependencyTool)
}

func registerPlanRemoveDependencyTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_remove_dependency", "Remove a dependency between two plan items; auto-unblocks the item if all remaining deps are done",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
			mcplib.String("item_id", "Item ID to unblock", mcplib.Required()),
			mcplib.String("depends_on_item_id", "Item ID to remove from dependencies", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			err := service.RemoveDependency(ctx,
				req.StringOr("plan_id", ""),
				req.StringOr("item_id", ""),
				req.StringOr("depends_on_item_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"status": "dependency_removed"}), nil
		},
	)
}
