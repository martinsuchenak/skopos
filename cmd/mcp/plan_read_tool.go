package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanReadTool)
}

func registerPlanReadTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_read", "Get a plan with all items, or a single item if item_id is provided",
			mcplib.String("id", "Plan ID", mcplib.Required()),
			mcplib.String("item_id", "Optional: return just this item instead of the full plan (use to check if a previously blocked item is now ready)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			plan, err := service.GetPlan(ctx, req.StringOr("id", ""))
			if err != nil {
				return nil, mcplib.NewToolErrorInternal(err.Error())
			}
			if itemID := req.StringOr("item_id", ""); itemID != "" {
				for _, item := range plan.Items {
					if item.ID == itemID {
						return mcplib.NewToolResponseJSON(item), nil
					}
				}
				return nil, mcplib.NewToolErrorInvalidParams("item not found in plan")
			}
			return mcplib.NewToolResponseJSON(plan), nil
		},
	)
}
