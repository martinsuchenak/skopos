package mcp

import (
	"context"
	"encoding/json"

	"github.com/martinsuchenak/skopos/internal/status"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterTool(registerReportStatusTool)
}

func registerReportStatusTool(server *mcplib.Server, service *status.Service) {
	server.RegisterTool(
		mcplib.NewTool("report_status", "Report an AI agent status update to Skopos",
			mcplib.String("agent_id", "Stable agent identifier", mcplib.Required()),
			mcplib.String("agent_type", "Agent implementation, such as codex, gemini, claude-code, opencode, kiro, or praxis", mcplib.Required()),
			mcplib.String("workspace_id", "Workspace ID (preferred over workspace)"),
			mcplib.String("workspace", "Workspace or repository path"),
			mcplib.String("status", "One of: pending, thinking, planning, running, editing, testing, waiting, blocked, paused, handoff, succeeded, failed, cancelled. (stuck and orphaned are server-set only — do not use)", mcplib.Required()),
			mcplib.String("session_id", "Optional session identifier"),
			mcplib.Integer("progress", "Optional progress percentage from 0 to 100"),
			mcplib.Integer("step_current", "Optional current step"),
			mcplib.Integer("step_total", "Optional total steps"),
			mcplib.String("message", "Short status message"),
			mcplib.String("snippet", "Short output snippet"),
			mcplib.String("metadata", "Optional metadata JSON string"),
			mcplib.String("git_branch", "Optional current git branch name"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := status.ReportInput{
				SessionID: req.StringOr("session_id", ""),
				AgentID:   req.StringOr("agent_id", ""),
				AgentType: req.StringOr("agent_type", ""),
				Workspace: req.StringOr("workspace_id", req.StringOr("workspace", "")),
				Status:    status.Status(req.StringOr("status", "")),
				Message:   req.StringOr("message", ""),
				Snippet:   req.StringOr("snippet", ""),
				Metadata:  map[string]any{},
			}
			if progress, err := req.Int("progress"); err == nil {
				input.Progress = &progress
			}
			if stepCurrent, err := req.Int("step_current"); err == nil {
				input.StepCurrent = &stepCurrent
			}
			if stepTotal, err := req.Int("step_total"); err == nil {
				input.StepTotal = &stepTotal
			}
			if rawMetadata := req.StringOr("metadata", ""); rawMetadata != "" {
				if err := json.Unmarshal([]byte(rawMetadata), &input.Metadata); err != nil {
					return nil, mcplib.NewToolErrorInvalidParams("metadata must be a JSON object")
				}
			}
			input.GitBranch = req.StringOr("git_branch", "")

			result, err := service.Report(ctx, input)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(result), nil
		},
	)
}
