package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanAddDependencyTool)
}

func registerPlanAddDependencyTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_add_dependency", "Add a dependency between two plan items; the dependent item is auto-blocked until its dependency is done",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
			mcplib.String("item_id", "Item ID that will be blocked", mcplib.Required()),
			mcplib.String("depends_on_item_id", "Item ID that must be done first", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			err := service.AddDependency(ctx,
				req.StringOr("plan_id", ""),
				req.StringOr("item_id", ""),
				req.StringOr("depends_on_item_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"status": "dependency_added"}), nil
		},
	)
}
