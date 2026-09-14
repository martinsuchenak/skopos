package status

import (
	"context"
	"errors"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func scopedCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k1", Name: "ci", Workspaces: map[string]struct{}{"ws-a": {}},
	})
}

func TestStatusWorkspaceScopeMatrix(t *testing.T) {
	svc := NewService(testStorage(t))
	ctx := context.Background()

	// Reports must target an accessible workspace; the resolved key is
	// stamped into metadata server-side as provenance.
	result, err := svc.Report(scopedCtx(), ReportInput{
		AgentID: "a", AgentType: "zcode", Workspace: "ws-a", Status: StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := svc.GetSession(ctx, result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Agents) == 0 || detail.Agents[0].Metadata == nil {
		t.Fatal("expected stamped metadata on the agent state")
	}
	if key, ok := detail.Agents[0].Metadata["auth_key"].(map[string]any); !ok || key["name"] != "ci" {
		t.Fatalf("expected auth_key provenance, got %+v", detail.Agents[0].Metadata)
	}

	// Out-of-scope report rejected.
	if _, err := svc.Report(scopedCtx(), ReportInput{
		AgentID: "a", AgentType: "zcode", Workspace: "ws-b", Status: StatusRunning,
	}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("report out of scope: %v", err)
	}

	// Foreign sessions are invisible by id (404 semantics) and absent from
	// unscoped lists.
	foreign, err := svc.Report(ctx, ReportInput{
		AgentID: "a", AgentType: "zcode", Workspace: "ws-b", Status: StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSession(scopedCtx(), foreign.SessionID); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("get foreign session: %v", err)
	}
	if err := svc.DeleteSession(scopedCtx(), foreign.SessionID); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("delete foreign session: %v", err)
	}
	sessions, err := svc.ListSessions(scopedCtx(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range sessions {
		if sess.Workspace != "ws-a" {
			t.Fatalf("unscoped list leaked session from %q", sess.Workspace)
		}
	}
}
