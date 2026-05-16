package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanUpdateItemTool)
}

func registerPlanUpdateItemTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_update_item", "Update an item's status or claim it",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
			mcplib.String("item_id", "Item ID", mcplib.Required()),
			mcplib.String("status", "New status: pending, in_progress, done, or blocked"),
			mcplib.String("claimed_by_agent_id", "Agent ID claiming the item; pass empty string to release"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := plans.UpdateItemInput{
				Status: plans.ItemStatus(req.StringOr("status", "")),
			}
			if claimed := req.StringOr("claimed_by_agent_id", "\x00"); claimed != "\x00" {
				input.ClaimedByAgentID = &claimed
			}
			item, err := service.UpdateItem(ctx,
				req.StringOr("plan_id", ""),
				req.StringOr("item_id", ""),
				input,
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(item), nil
		},
	)
}
