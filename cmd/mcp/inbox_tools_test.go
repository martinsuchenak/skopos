package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
)

func TestInboxToolsRegistered(t *testing.T) {
	found := false
	for range inboxToolRegistrations {
		found = true
		break
	}
	if !found {
		t.Fatal("inboxToolRegistrations should not be empty")
	}
}

// TestInboxWorkflowEndToEnd drives the full agent flow through the MCP
// surface: capture (as root) -> list -> read -> claim -> enrich -> plan ->
// convert -> context shows the inbox.
func TestInboxWorkflowEndToEnd(t *testing.T) {
	// One shared DB: the plan created mid-workflow must be visible to the
	// inbox service that validates the conversion link.
	sqlDB := mustOpenDB(t)
	h := NewMCPHandler(
		status.NewService(status.NewStorage(sqlDB)),
		blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB)),
		inbox.NewService(inbox.NewStorage(sqlDB)),
		nil, nil,
	)
	// Unauthenticated handler = system principal (root-equivalent), matching
	// how the other e2e tools tests drive the MCP surface.
	sid := initialize(t, h)

	// Capture.
	r := callTool(t, h, sid, 2, "inbox_create", map[string]any{
		"workspace_id": "ws-a", "title": "Implement user management",
		"content": "rough idea\nsee internal/auth", "tags": []string{"Auth", "auth"}, "author_agent_id": "agent-t",
	})
	if r.Result.IsError {
		t.Fatalf("inbox_create: %s", r.Result.Content[0].Text)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &created); err != nil || created.ID == "" {
		t.Fatalf("inbox_create result: %v (%s)", err, r.Result.Content[0].Text)
	}

	// List (default open) finds it.
	r = callTool(t, h, sid, 3, "inbox_list", map[string]any{"workspace_id": "ws-a"})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, "Implement user management") {
		t.Fatalf("inbox_list: %s", r.Result.Content[0].Text)
	}

	// Read returns raw markdown, not HTML.
	r = callTool(t, h, sid, 4, "inbox_read", map[string]any{"item_id": created.ID})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, "rough idea") {
		t.Fatalf("inbox_read: %s", r.Result.Content[0].Text)
	}

	// Claim.
	r = callTool(t, h, sid, 5, "inbox_claim", map[string]any{"item_id": created.ID, "agent_id": "agent-t"})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, "in_progress") {
		t.Fatalf("inbox_claim: %s", r.Result.Content[0].Text)
	}

	// Enrich (append convention is guidance; here just new content).
	r = callTool(t, h, sid, 6, "inbox_update", map[string]any{
		"item_id": created.ID,
		"content": "rough idea\nsee internal/auth\n\n## Enrichment — agent-t, 2026-09-19\n- plan outline here",
	})
	if r.Result.IsError {
		t.Fatalf("inbox_update: %s", r.Result.Content[0].Text)
	}

	// Author a plan, then convert.
	r = callTool(t, h, sid, 7, "plan_create", map[string]any{
		"workspace_id": "ws-a", "name": "User management plan", "author_agent_id": "agent-t",
	})
	var plan struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &plan); err != nil || plan.ID == "" {
		t.Fatalf("plan_create: %v (%s)", err, r.Result.Content[0].Text)
	}
	r = callTool(t, h, sid, 8, "inbox_convert", map[string]any{"item_id": created.ID, "plan_id": plan.ID})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, "converted") {
		t.Fatalf("inbox_convert: %s", r.Result.Content[0].Text)
	}

	// The context snapshot's inbox section no longer lists the converted item
	// (open only) and does not error.
	r = callTool(t, h, sid, 9, "skopos_context", map[string]any{"workspace_id": "ws-a"})
	if r.Result.IsError {
		t.Fatalf("skopos_context: %s", r.Result.Content[0].Text)
	}
	var snap struct {
		Inbox struct {
			Open  int `json:"open"`
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		} `json:"inbox"`
	}
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &snap); err != nil {
		t.Fatalf("snapshot decode: %v (%s)", err, r.Result.Content[0].Text)
	}
	if snap.Inbox.Open != 0 {
		t.Fatalf("converted item should not be open in snapshot: %+v", snap.Inbox)
	}

}

// TestInboxToolErrorsClassifiedAsClientMistakes pins toolError's mapping:
// inbox client errors (unknown id, claim conflict, frozen edit) must be
// invalid-params over MCP, not internal failures.
func TestInboxToolErrorsClassifiedAsClientMistakes(t *testing.T) {
	sqlDB := mustOpenDB(t)
	h := NewMCPHandler(
		status.NewService(status.NewStorage(sqlDB)),
		blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB)),
		inbox.NewService(inbox.NewStorage(sqlDB)),
		nil, nil,
	)
	sid := initialize(t, h)

	// Unknown id: invalid-params (-32602), not internal (-32603).
	e := callToolExpectError(t, h, sid, 20, "inbox_read", map[string]any{"item_id": "no-such-item"})
	if e.Code != -32602 {
		t.Fatalf("unknown id should classify as invalid-params (-32602), got %d: %s", e.Code, e.Message)
	}
	if !strings.Contains(e.Message, "not found") {
		t.Fatalf("unknown id message: %s", e.Message)
	}

	created := callTool(t, h, sid, 21, "inbox_create", map[string]any{
		"workspace_id": "ws-a", "title": "conflict probe", "author_agent_id": "t",
	})
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Result.Content[0].Text), &item); err != nil {
		t.Fatalf("create: %v (%s)", err, created.Result.Content[0].Text)
	}
	callTool(t, h, sid, 22, "inbox_claim", map[string]any{"item_id": item.ID, "agent_id": "a1"})
	conflict := callToolExpectError(t, h, sid, 23, "inbox_claim", map[string]any{"item_id": item.ID, "agent_id": "a2"})
	if conflict.Code != -32602 {
		t.Fatalf("claim conflict should classify as invalid-params (-32602), got %d: %s", conflict.Code, conflict.Message)
	}
	if !strings.Contains(conflict.Message, "already claimed") {
		t.Fatalf("claim conflict message: %s", conflict.Message)
	}
}

// TestInboxPriorityAddressing drives the by-number reference: workspace_id +
// priority stand in for item_id, a missing reference is invalid-params, and a
// stale number (post-reorder) is not-found with a pointer to inbox_list.
func TestInboxPriorityAddressing(t *testing.T) {
	sqlDB := mustOpenDB(t)
	h := NewMCPHandler(
		status.NewService(status.NewStorage(sqlDB)),
		blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB)),
		inbox.NewService(inbox.NewStorage(sqlDB)),
		nil, nil,
	)
	sid := initialize(t, h)

	// Two prioritized items: #1 audit, #2 search.
	callTool(t, h, sid, 2, "inbox_create", map[string]any{
		"workspace_id": "ws-a", "title": "audit log", "author_agent_id": "t", "priority": 1,
	})
	callTool(t, h, sid, 3, "inbox_create", map[string]any{
		"workspace_id": "ws-a", "title": "search notes", "author_agent_id": "t", "priority": 2,
	})

	// Read by number: #2 is the search item.
	r := callTool(t, h, sid, 4, "inbox_read", map[string]any{"workspace_id": "ws-a", "priority": 2})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, "search notes") {
		t.Fatalf("inbox_read by priority: %s", r.Result.Content[0].Text)
	}

	// Claim by number moves it to in_progress.
	r = callTool(t, h, sid, 5, "inbox_claim", map[string]any{"workspace_id": "ws-a", "priority": 2, "agent_id": "agent-t"})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, "in_progress") {
		t.Fatalf("inbox_claim by priority: %s", r.Result.Content[0].Text)
	}

	// No reference at all: invalid-params naming both addressing forms.
	e := callToolExpectError(t, h, sid, 6, "inbox_read", map[string]any{})
	if e.Code != -32602 || !strings.Contains(e.Message, "workspace_id + priority") {
		t.Fatalf("missing reference: %d %s", e.Code, e.Message)
	}

	// Half a reference (priority without workspace) is the same error.
	e = callToolExpectError(t, h, sid, 7, "inbox_read", map[string]any{"priority": 1})
	if e.Code != -32602 {
		t.Fatalf("half reference: %d %s", e.Code, e.Message)
	}

	// A number the board does not have is not-found with guidance.
	e = callToolExpectError(t, h, sid, 8, "inbox_read", map[string]any{"workspace_id": "ws-a", "priority": 9})
	if e.Code != -32602 || !strings.Contains(e.Message, "inbox_list") {
		t.Fatalf("stale number: %d %s", e.Code, e.Message)
	}

	// item_id still wins when both forms are passed: the pair alone would
	// have resolved to the audit item, but the unknown id is what answers.
	e = callToolExpectError(t, h, sid, 9, "inbox_read", map[string]any{"item_id": "no-such-item", "workspace_id": "ws-a", "priority": 1})
	if e.Code != -32602 || !strings.Contains(e.Message, "no-such-item") {
		t.Fatalf("item_id precedence: %d %s", e.Code, e.Message)
	}
}
