package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanAddItemTool)
}

func registerPlanAddItemTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_add_item", "Add a work item to a plan",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
			mcplib.String("title", "Item title", mcplib.Required()),
			mcplib.String("description", "Optional description"),
			mcplib.String("phase", "Optional phase label for grouping (e.g. research, implement, test)"),
			mcplib.Integer("position", "Insert position (0-based); omit to append to end"),
			mcplib.StringArray("depends_on", "Item IDs this item depends on; auto-blocked until all deps are done"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			var pos *int
			if v, err := req.Int("position"); err == nil {
				pos = &v
			}
			item, err := service.AddItem(ctx, req.StringOr("plan_id", ""), plans.CreateItemInput{
				Title:       req.StringOr("title", ""),
				Description: req.StringOr("description", ""),
				Phase:       req.StringOr("phase", ""),
				Position:    pos,
				DependsOn:   req.StringSliceOr("depends_on", nil),
			})
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(item), nil
		},
	)
}
