package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func keyCtx(p *Principal) context.Context { return WithPrincipal(context.Background(), p) }

func scope(ws ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range ws {
		out[w] = struct{}{}
	}
	return out
}

// The admin permission by level: root and internal callers pass everything;
// a key needs the flag and enough scope; an approver or a plain key never
// passes; a run principal never passes even with the flag set.
func TestRequireAdminLevels(t *testing.T) {
	root := keyCtx(&Principal{Root: true})
	internal := context.Background()
	plain := keyCtx(&Principal{KeyID: "k", Workspaces: scope("ws-a")})
	approver := keyCtx(&Principal{KeyID: "k", Approver: true, AllWorkspaces: true})
	adminA := keyCtx(&Principal{KeyID: "k", Admin: true, Workspaces: scope("ws-a")})
	adminAB := keyCtx(&Principal{KeyID: "k", Admin: true, Workspaces: scope("ws-a", "ws-b")})
	adminAll := keyCtx(&Principal{KeyID: "k", Admin: true, AllWorkspaces: true})
	run := keyCtx(&Principal{KeyID: "run:r1", Admin: true, AllWorkspaces: true, Run: &RunIdentity{ID: "r1", WorkspaceID: "ws-a"}})

	cases := []struct {
		name                     string
		ctx                      context.Context
		wsA, wsB, groupAB, fleet bool // may it administer: ws-a, ws-b, the group {ws-a, ws-b}, the fleet
	}{
		{"root", root, true, true, true, true},
		{"internal", internal, true, true, true, true},
		{"plain key", plain, false, false, false, false},
		{"approver with all workspaces", approver, false, false, false, false},
		{"admin over ws-a", adminA, true, false, false, false},
		{"admin over ws-a and ws-b", adminAB, true, true, true, false},
		{"admin over all workspaces", adminAll, true, true, true, true},
		{"run principal", run, false, false, false, false},
	}
	for _, c := range cases {
		got := []bool{
			RequireAdmin(c.ctx, "ws-a") == nil,
			RequireAdmin(c.ctx, "ws-b") == nil,
			RequireAdminAll(c.ctx, []string{"ws-a", "ws-b"}) == nil,
			RequireFleetAdmin(c.ctx) == nil,
		}
		want := []bool{c.wsA, c.wsB, c.groupAB, c.fleet}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: level %d = %v, want %v", c.name, i, got[i], want[i])
			}
		}
	}
	// Every refusal is ErrAdminRequired (handlers map it to 403).
	if err := RequireAdmin(adminA, "ws-b"); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("out-of-scope admin: %v", err)
	}
	// A group with no workspaces is bounded by nothing: fleet level.
	if RequireAdminAll(adminAB, nil) == nil || RequireAdminAll(adminAll, nil) != nil {
		t.Fatal("an empty set of workspaces needs the fleet level")
	}
}

type fakeRuns map[string]*RunIdentity

func (f fakeRuns) LookupRunToken(_ context.Context, hash string) (*RunIdentity, error) {
	return f[hash], nil
}

// A run's agent token authenticates as that run: its own workspace only,
// the agent preset (MCP, never REST), no permissions — and never root,
// whatever reads an empty key id as root.
func TestRunTokenPrincipal(t *testing.T) {
	const token = RunTokenPrefix + "abc"
	a := NewAuthenticator("root-secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if p := a.Authenticate(req); p != nil {
		t.Fatalf("without a run lookup a run token is unknown: %+v", p)
	}
	a.SetRunLookup(fakeRuns{HashKey(token): {
		ID: "r1", WorkspaceID: "ws-a", ItemID: "i1", PlanID: "p1", Stage: "implement", Runner: "opencode-glm", WorkerID: "w1",
	}})
	p := a.Authenticate(req)
	if p == nil {
		t.Fatal("the run token must authenticate")
	}
	if p.Kind() != KindRun || p.IsRoot() || p.Root || p.Approver || p.Admin || p.Delegate || p.AllWorkspaces {
		t.Fatalf("run principal has more than a run: %+v", p)
	}
	if p.KeyID != "run:r1" {
		t.Fatalf("a run principal's key id must never be empty (empty reads as root): %q", p.KeyID)
	}
	if !p.CanAccess("ws-a") || p.CanAccess("ws-b") {
		t.Fatal("scope must be the run's workspace only")
	}
	if !p.Capped() || !p.AllowsSurface(SurfaceMCP) || p.AllowsSurface(SurfaceREST) {
		t.Fatalf("a run uses MCP, never REST: %v", p.CapabilityList())
	}
	if ok, _ := p.AllowsRequest(http.MethodPost, "/api/runs/claim"); ok {
		t.Fatal("a run token must not reach the runs API")
	}
	actor, keyID := Actor(keyCtx(p))
	if actor != "run r1 (opencode-glm on w1)" || keyID != "run:r1" {
		t.Fatalf("actor: %q %q", actor, keyID)
	}
	if RunOf(keyCtx(p)) == nil || RunOf(keyCtx(&Principal{KeyID: "k"})) != nil || RunOf(context.Background()) != nil {
		t.Fatal("RunOf is the run principal's run, nil otherwise")
	}

	// A server without authentication treats everyone as root — but a live
	// run token is still a run there, not root.
	open := NewAuthenticator("", nil)
	open.SetRunLookup(fakeRuns{HashKey(token): {ID: "r1", WorkspaceID: "ws-a", Stage: "plan", WorkerID: "w1"}})
	if p := open.Authenticate(req); p == nil || p.Root || p.Kind() != KindRun || p.CanAccess("ws-b") {
		t.Fatalf("a run token on an unauthenticated server must stay a run: %+v", p)
	}
	if p := open.Authenticate(httptest.NewRequest(http.MethodGet, "/api/whoami", nil)); p == nil || !p.Root {
		t.Fatalf("no credential on an unauthenticated server is root: %+v", p)
	}

	// An unknown run token is refused; it is never tried as an API key.
	other := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	other.Header.Set("Authorization", "Bearer "+RunTokenPrefix+"nope")
	if p := a.Authenticate(other); p != nil {
		t.Fatalf("unknown run token: %+v", p)
	}
	// Also on a server without authentication: a token whose run has ended
	// (or was cancelled, or lost its lease) must not make its agent root.
	if p := open.Authenticate(other); p != nil {
		t.Fatalf("a run token that no longer resolves on an unauthenticated server: %+v", p)
	}
}

func TestPrincipalKind(t *testing.T) {
	var none *Principal
	for want, p := range map[Kind]*Principal{
		KindSystem: none,
		KindRoot:   {Root: true},
		KindKey:    {KeyID: "k"},
		KindRun:    {KeyID: "run:r", Run: &RunIdentity{ID: "r"}},
	} {
		if got := p.Kind(); got != want {
			t.Errorf("kind = %s, want %s", got, want)
		}
	}
}
