package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanRemovePlanDependencyTool)
}

func registerPlanRemovePlanDependencyTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_remove_plan_dependency", "Remove a plan-to-plan dependency; auto-unblocks the plan if all remaining deps are completed",
			mcplib.String("plan_id", "Plan ID to unblock", mcplib.Required()),
			mcplib.String("depends_on_plan_id", "Plan ID to remove from dependencies", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			err := service.RemovePlanDependency(ctx,
				req.StringOr("plan_id", ""),
				req.StringOr("depends_on_plan_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"status": "plan_dependency_removed"}), nil
		},
	)
}
