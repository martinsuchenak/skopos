package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanArchiveTool)
}

func registerPlanArchiveTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_archive", "Archive a plan (soft delete: sets status to 'archived'). Prefer this over deleting when a plan's work is done or abandoned; archived plans are kept for the record but no longer active.",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			planID := req.StringOr("plan_id", "")
			if err := service.UpdatePlan(ctx, planID, plans.UpdatePlanInput{Status: plans.PlanArchived}); err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"id": planID, "status": "archived"}), nil
		},
	)
}
