package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/status"
)

// inbox_questions: an item's record by id, and the decision log without
// an item.
func TestInboxQuestionsTool(t *testing.T) {
	sqlDB := mustOpenDB(t)
	inboxSvc := inbox.NewService(inbox.NewStorage(sqlDB))
	qaSvc := qa.NewService(qa.NewStorage(sqlDB))
	inboxSvc.SetQuestionLog(qaSvc)
	h := NewMCPHandler(
		status.NewService(status.NewStorage(sqlDB)),
		blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB)),
		inboxSvc,
		nil, nil,
	)
	sid := initialize(t, h)
	item, err := inboxSvc.CreateItem(context.Background(), inbox.CreateInput{WorkspaceID: "ws-a", Title: "cache", AuthorAgentID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qaSvc.Ask(context.Background(), qa.AskInput{WorkspaceID: "ws-a", ItemID: item.ID, Asker: qa.AskerPlanner,
		Questions: []itemtext.Question{{Text: "Which backend?", Options: []string{"Redis", "SQLite"}}}}); err != nil {
		t.Fatal(err)
	}
	r := callTool(t, h, sid, 2, "inbox_questions", map[string]any{"item_id": item.ID})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, `"state":"open"`) || !strings.Contains(r.Result.Content[0].Text, "Which backend?") {
		t.Fatalf("item record: %s", r.Result.Content[0].Text)
	}
	r = callTool(t, h, sid, 3, "inbox_questions", map[string]any{"workspace_id": "ws-a", "state": "open"})
	if r.Result.IsError || !strings.Contains(r.Result.Content[0].Text, `"total":1`) || !strings.Contains(r.Result.Content[0].Text, `"item_title":"cache"`) {
		t.Fatalf("decision log: %s", r.Result.Content[0].Text)
	}
	r = callTool(t, h, sid, 4, "inbox_questions", map[string]any{"from": "yesterday"})
	if !r.Result.IsError {
		t.Fatalf("a bad date is an input error: %s", r.Result.Content[0].Text)
	}
}
