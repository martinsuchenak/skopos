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
			// plan_id is validated in the handler (not via Required) so the
			// legacy `id` spelling can be accepted as an alias.
			mcplib.String("plan_id", "Plan ID"),
			mcplib.String("id", "Alias of plan_id (accepted for compatibility with older clients)"),
			mcplib.String("item_id", "Optional: return just this item instead of the full plan (use to check if a previously blocked item is now ready)"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			planID := req.StringOr("plan_id", "")
			if planID == "" {
				planID = req.StringOr("id", "")
			}
			if planID == "" {
				return nil, mcplib.NewToolErrorInvalidParams("missing required parameter: plan_id")
			}
			plan, err := service.GetPlan(ctx, planID)
			if err != nil {
				return nil, toolError(err)
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
