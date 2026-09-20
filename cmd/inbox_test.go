package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
)

func TestInboxCmdExists(t *testing.T) {
	cmd := inboxCmd()
	if cmd == nil {
		t.Fatal("inboxCmd should not return nil")
	}
	if cmd.Name != "inbox" {
		t.Fatalf("expected inbox command, got %q", cmd.Name)
	}
	if len(cmd.Commands) == 0 {
		t.Fatal("expected inbox subcommands")
	}
}

func mustMarshalTB(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestInboxLifecycleAgainstServer(t *testing.T) {
	ts := keysTestServer(t, "rootkey1")
	ctx := context.Background()
	ws := "github.com/o/a"

	// Capture with mixed-case and invalid tags.
	item, err := inboxPost(ctx, ts.URL, "rootkey1", inbox.CreateInput{
		WorkspaceID:   ws,
		Title:         "Implement user management",
		Content:       "rough notes with **markdown**",
		Tags:          []string{"Auth", "auth!", "ui"},
		AuthorAgentID: "cli-user",
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if item.Status != inbox.StatusOpen {
		t.Fatalf("new item should be open: %s", item.Status)
	}
	// "auth!" is invalid (charset) and dropped; "Auth" lowercases; "ui" kept.
	if len(item.Tags) != 2 || item.Tags[0] != "auth" || item.Tags[1] != "ui" {
		t.Fatalf("tags normalized: %v", item.Tags)
	}

	// List (open only) carries excerpts, not content.
	items, err := inboxGetList(ctx, ts.URL, "rootkey1", string(inbox.StatusOpen), "", ws)
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %d", err, len(items))
	}
	if items[0].Content != "" || items[0].Excerpt == "" {
		t.Fatalf("list row should be excerpt-only: %+v", items[0])
	}

	// Tag filter via the server (normalized to lowercase).
	tagged, err := inboxGetList(ctx, ts.URL, "rootkey1", "", "AUTH", ws)
	if err != nil || len(tagged) != 1 {
		t.Fatalf("tag filter: %v %d", err, len(tagged))
	}

	// Detail read renders HTML.
	detail, err := inboxGetOne(ctx, ts.URL, "rootkey1", item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(detail.ContentHTML, "<strong>") {
		t.Fatalf("content_html should render markdown: %q", detail.ContentHTML)
	}

	// Enrich, claim, author a plan, convert.
	newTags := []string{"refactor"}
	if err := inboxPatch(ctx, ts.URL, "rootkey1", item.ID, inbox.UpdateInput{Content: "enriched\n\n## Enrichment — cli, today\n", Tags: &newTags}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := inboxAction(ctx, ts.URL, "rootkey1", item.ID, "claim", mustMarshalTB(t, map[string]string{"agent_id": "cli-agent"}), "claiming item"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	plan, err := plansPost(ctx, ts.URL, "rootkey1", plans.CreatePlanInput{
		Name: "User management", AuthorAgentID: "cli-agent", WorkspaceID: ws,
	})
	if err != nil {
		t.Fatalf("plan create: %v", err)
	}
	converted, err := inboxConvertItem(ctx, ts.URL, "rootkey1", item.ID, plan.ID)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if converted.Status != inbox.StatusConverted || converted.Plan == nil || converted.Plan.Name != "User management" {
		t.Fatalf("converted: %+v", converted)
	}

	// Second convert fails (already converted).
	foreign, err := plansPost(ctx, ts.URL, "rootkey1", plans.CreatePlanInput{
		Name: "Foreign", AuthorAgentID: "cli-agent", WorkspaceID: "github.com/o/b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inboxConvertItem(ctx, ts.URL, "rootkey1", item.ID, foreign.ID); err == nil {
		t.Fatal("second convert should fail (already converted)")
	}

	// Discard + delete a second item.
	second, err := inboxPost(ctx, ts.URL, "rootkey1", inbox.CreateInput{WorkspaceID: ws, Title: "temp", AuthorAgentID: "cli-user"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inboxAction(ctx, ts.URL, "rootkey1", second.ID, "discard", []byte("{}"), "discarding item"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	discarded, err := inboxGetOne(ctx, ts.URL, "rootkey1", second.ID)
	if err != nil || discarded.Status != inbox.StatusDiscarded {
		t.Fatalf("discarded: %v %+v", err, discarded)
	}
	if err := inboxDelete(ctx, ts.URL, "rootkey1", second.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := inboxGetOne(ctx, ts.URL, "rootkey1", second.ID); err == nil {
		t.Fatal("deleted item should be gone")
	}
}

func TestInboxContentSources(t *testing.T) {
	if c, err := inboxContent("inline", ""); err != nil || c != "inline" {
		t.Fatalf("inline: %q %v", c, err)
	}

	path := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(path, []byte("# from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := inboxContent("", path); err != nil || c != "# from file\n" {
		t.Fatalf("file: %q %v", c, err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = w.WriteString("piped"); _ = w.Close() }()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
	if c, err := inboxContent("", "-"); err != nil || c != "piped" {
		t.Fatalf("stdin: %q %v", c, err)
	}
}
