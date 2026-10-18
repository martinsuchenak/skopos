package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/status"
)

// workspace_pipeline reads the effective routing; there is no write tool.
func TestWorkspacePipelineTool(t *testing.T) {
	sqlDB := mustOpenDB(t)
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, created_at) VALUES ('ws-a', '')`); err != nil {
		t.Fatal(err)
	}
	svc := pipeline.NewService(pipeline.NewStorage(sqlDB))
	if _, err := svc.Put(context.Background(), pipeline.KindWorkspace, "ws-a", pipeline.Settings{Stages: map[string]pipeline.StageSetting{
		"review": {Members: []runs.Member{{Runner: "claude", Focus: "security"}, {Runner: "opencode-glm"}}, Policy: "majority"},
	}}, ""); err != nil {
		t.Fatal(err)
	}
	SetPipelineReader(svc)
	t.Cleanup(func() { SetPipelineReader(nil) })
	h := NewMCPHandler(status.NewService(status.NewStorage(sqlDB)), blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB)), inbox.NewService(inbox.NewStorage(sqlDB)), nil, nil)
	sid := initialize(t, h)
	r := callTool(t, h, sid, 2, "workspace_pipeline", map[string]any{"workspace_id": "ws-a"})
	text := r.Result.Content[0].Text
	if r.Result.IsError || !strings.Contains(text, `"runners":"security@claude,opencode-glm"`) || !strings.Contains(text, `"policy":"majority"`) || !strings.Contains(text, `"agents_default":"panel"`) {
		t.Fatalf("workspace_pipeline: %s", text)
	}
	if e := callToolExpectError(t, h, sid, 3, "workspace_pipeline", map[string]any{"workspace_id": "nope"}); !strings.Contains(e.Message, "not registered") {
		t.Fatalf("unknown workspace: %+v", e)
	}
}
