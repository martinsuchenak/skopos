package status

import (
	"strings"
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
	if _, err := svc.GetSession(scopedCtx(), foreign.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get foreign session: %v", err)
	}
	_, foreignErr := svc.GetSession(scopedCtx(), foreign.SessionID)
	_, unknownErr := svc.GetSession(scopedCtx(), "no-such-session")
	if strings.Replace(foreignErr.Error(), foreign.SessionID, "X", 1) != strings.Replace(unknownErr.Error(), "no-such-session", "X", 1) {
		t.Fatalf("foreign/unknown session errors differ beyond the id: %q vs %q", foreignErr, unknownErr)
	}
	if err := svc.DeleteSession(scopedCtx(), foreign.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete foreign session: %v", err)
	}
	if _, err := svc.ListEvents(scopedCtx(), foreign.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign ListEvents: %v", err)
	}
	// Session takeover (fourth pentest round, vuln-0001): attaching to a
	// foreign session with a declared workspace is rejected, and the
	// binding is immutable even for in-scope reports.
	if _, err := svc.Report(scopedCtx(), ReportInput{
		AgentID: "evil", AgentType: "zcode", Workspace: "ws-a",
		SessionID: foreign.SessionID, Status: StatusRunning,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign session attach must be rejected: %v", err)
	}
	if sess, err := svc.GetSession(ctx, foreign.SessionID); err != nil || sess.Workspace != "ws-b" {
		t.Fatalf("foreign session must keep its binding: %+v %v", sess, err)
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

func TestStatusListEventsAndActiveAgentsScoped(t *testing.T) {
	svc := NewService(testStorage(t))
	ctx := context.Background()

	own, err := svc.Report(scopedCtx(), ReportInput{
		AgentID: "a", AgentType: "zcode", Workspace: "ws-a", Status: StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := svc.Report(ctx, ReportInput{
		AgentID: "f", AgentType: "zcode", Workspace: "ws-b", Status: StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}

	// ListEvents on a foreign session: uniform not-found (no oracle).
	if _, err := svc.ListEvents(scopedCtx(), foreign.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign ListEvents: %v", err)
	}
	if _, err := svc.ListEvents(scopedCtx(), own.SessionID); err != nil {
		t.Fatalf("own ListEvents: %v", err)
	}

	// ListActiveAgents returns only the caller's workspace's agents.
	agents, err := svc.ListActiveAgents(scopedCtx())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Workspace != "ws-a" {
			t.Fatalf("ListActiveAgents leaked agent from %q", a.Workspace)
		}
	}
	all, err := svc.ListActiveAgents(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("root/internal must see all agents: %+v %v", all, err)
	}
}
