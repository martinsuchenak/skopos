package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/pipeline"
	mcplib "github.com/paularlott/mcp"
)

// PipelineReader is the read side of the pipeline settings service. Agents
// may see which agents run each stage; they never change it (MCP has no
// write tool — the dashboard, CLI and REST do, approver-gated).
type PipelineReader interface {
	Get(ctx context.Context, kind, id string) (*pipeline.View, error)
}

var pipelineReader PipelineReader

// SetPipelineReader registers the workspace_pipeline tool on handlers built
// afterwards (serve.go calls it before NewMCPHandler).
func SetPipelineReader(r PipelineReader) { pipelineReader = r }

func registerPipelineTool(server *mcplib.Server, r PipelineReader) {
	registerTool(server,
		mcplib.NewTool("workspace_pipeline",
			"Read which agents run each workflow stage (plan, compare, synthesize, implement, review, qa) for a workspace: the effective runners with their focus briefs, the decision rule, the agents default for items, and where each comes from (the workspace or a group). Read-only.",
			wsParam(),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			ws, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			v, err := r.Get(ctx, pipeline.KindWorkspace, ws)
			if err != nil {
				return nil, toolError(err)
			}
			stages := map[string]any{}
			for stage, e := range v.Effective {
				entry := map[string]any{"runners": pipeline.FormatMembers(e.Members), "source": e.Source}
				if e.Policy != "" {
					entry["policy"] = e.Policy
				}
				if e.EarlyExit {
					entry["early_exit"] = true
				}
				if e.TimeoutMS > 0 {
					entry["timeout_ms"] = e.TimeoutMS
				}
				stages[stage] = entry
			}
			agents := v.EffectiveAgentsDefault
			if agents == "" {
				agents = "panel"
			}
			out := map[string]any{"workspace_id": ws, "stages": stages, "agents_default": agents}
			if len(v.Conflicts) > 0 {
				out["conflicts"] = v.Conflicts
			}
			if len(v.UnknownRunners) > 0 {
				out["unknown_runners"] = v.UnknownRunners
			}
			return mcplib.NewToolResponseJSON(out), nil
		},
	)
}
