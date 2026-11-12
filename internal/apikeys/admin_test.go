package apikeys

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
)

// The admin flag travels the whole path — create, storage lookup, the
// principal, list and get — survives unrelated edits, and is revocable.
func TestAdminGrantLifecycle(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")

	result, err := svc.Create(ctx, CreateInput{Name: "ops", Workspaces: []string{"ws-a"}, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Key.Admin {
		t.Fatal("the created key must carry the admin flag")
	}
	authn := auth.NewAuthenticator("root-secret", st)
	p := authn.Authenticate(bearerRequest(result.Secret))
	if p == nil || !p.Admin || p.Approver {
		t.Fatalf("Authenticate must carry Admin (and only Admin) onto the principal: %+v", p)
	}
	keyCtx := auth.WithPrincipal(ctx, p)
	if auth.RequireAdmin(keyCtx, "ws-a") != nil || auth.RequireAdmin(keyCtx, "ws-b") == nil || auth.RequireFleetAdmin(keyCtx) == nil {
		t.Fatal("an admin over ws-a administers ws-a only")
	}
	keys, err := svc.List(ctx)
	if err != nil || len(keys) != 1 || !keys[0].Admin {
		t.Fatalf("list must show the flag: %+v %v", keys, err)
	}

	// A plain key has none; a default create never grants it.
	plain, err := svc.Create(ctx, CreateInput{Name: "plain", Workspaces: []string{"ws-a"}})
	if err != nil || plain.Key.Admin {
		t.Fatalf("a key is not an admin unless asked: %+v %v", plain, err)
	}
	if p := authn.Authenticate(bearerRequest(plain.Secret)); p == nil || p.Admin {
		t.Fatalf("plain key principal: %+v", p)
	}

	name := "ops2"
	updated, err := svc.Update(ctx, result.Key.ID, UpdateInput{Name: &name})
	if err != nil || !updated.Admin {
		t.Fatalf("the flag must survive a name edit: %+v %v", updated, err)
	}
	no := false
	if updated, err = svc.Update(ctx, result.Key.ID, UpdateInput{Admin: &no}); err != nil || updated.Admin {
		t.Fatalf("the flag must be revocable: %+v %v", updated, err)
	}
	if p := authn.Authenticate(bearerRequest(result.Secret)); p == nil || p.Admin {
		t.Fatalf("a removed grant applies at the next request: %+v", p)
	}
}

type recordedAudit struct{ entries []audit.RecordInput }

func (r *recordedAudit) Record(_ context.Context, in audit.RecordInput) error {
	r.entries = append(r.entries, in)
	return nil
}

// Key changes are audit events: create, update (what it was and what it is
// now), revoke and delete — with the permissions granted, and never the
// secret.
func TestKeyChangesAreAudited(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	rec := &recordedAudit{}
	svc.SetAuditRecorder(rec)
	seedWorkspace(t, st, "ws-a")
	ctx := WithVia(context.Background(), "cli")

	created, err := svc.Create(ctx, CreateInput{Name: "ops", Workspaces: []string{"ws-a"}, Admin: true, Preset: "integration"})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := svc.Update(ctx, created.Key.ID, UpdateInput{Approver: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, created.Key.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, created.Key.ID); err != nil { // already revoked: nothing happened, nothing recorded
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, created.Key.ID); err != nil {
		t.Fatal(err)
	}

	var actions []string
	for _, e := range rec.entries {
		actions = append(actions, e.Action)
		if e.EntityType != "api_key" || e.EntityID != created.Key.ID || e.Via != "cli" {
			t.Errorf("entry shape: %+v", e)
		}
		if strings.Contains(e.Notes, created.Secret) || strings.Contains(e.Notes, created.Secret[:20]) {
			t.Errorf("the audit note must never carry the secret: %q", e.Notes)
		}
	}
	if got := strings.Join(actions, ","); got != "key.create,key.update,key.revoke,key.delete" {
		t.Fatalf("actions: %s", got)
	}
	// A revoke and a delete say which key it was and what it held: the row
	// may be gone afterwards.
	for _, i := range []int{2, 3} {
		if n := rec.entries[i].Notes; !strings.Contains(n, "name=ops") || !strings.Contains(n, " admin") || !strings.Contains(n, " approver") {
			t.Fatalf("%s note must describe the key: %q", rec.entries[i].Action, n)
		}
	}
	if n := rec.entries[0].Notes; !strings.Contains(n, "name=ops") || !strings.Contains(n, "workspaces=ws-a") ||
		!strings.Contains(n, " admin") || strings.Contains(n, "approver") || !strings.Contains(n, "capabilities=") {
		t.Fatalf("create note: %q", n)
	}
	if n := rec.entries[1].Notes; !strings.Contains(n, "was: ") || !strings.Contains(n, "now: ") ||
		strings.Contains(strings.SplitN(n, "; now: ", 2)[0], "approver") || !strings.Contains(strings.SplitN(n, "; now: ", 2)[1], "approver") {
		t.Fatalf("update note must show the approver grant: %q", n)
	}
}

// whoami tells a key whether it is an admin, and a run's agent token which
// run it is.
func TestWhoamiAdminAndRun(t *testing.T) {
	h := testHandler(t)
	who := func(p *auth.Principal) map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		w := httptest.NewRecorder()
		h.Whoami(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("whoami: %d %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	admin := who(&auth.Principal{KeyID: "k1", Name: "ops", Admin: true, Workspaces: map[string]struct{}{"ws-a": {}}})
	if key, _ := admin["key"].(map[string]any); key == nil || key["admin"] != true {
		t.Fatalf("whoami must report admin: %v", admin)
	}
	run := who(&auth.Principal{
		KeyID: "run:r1", Name: "run r1", Workspaces: map[string]struct{}{"ws-a": {}}, Capabilities: map[string]struct{}{"mcp": {}},
		Run: &auth.RunIdentity{ID: "r1", WorkspaceID: "ws-a", ItemID: "i1", Stage: "review", Runner: "claude", WorkerID: "w1"},
	})
	if run["root"] != false {
		t.Fatalf("a run is never root: %v", run)
	}
	if key, _ := run["key"].(map[string]any); key == nil || key["admin"] != false || key["approver"] != false {
		t.Fatalf("a run holds no permission: %v", run)
	}
	if r, _ := run["run"].(map[string]any); r == nil || r["id"] != "r1" || r["workspace_id"] != "ws-a" || r["stage"] != "review" {
		t.Fatalf("whoami must name the run: %v", run)
	}
}

// A workspace group is part of the scope of every key that holds it, so
// creating, changing and deleting one are audit events too: a workspace
// added to a group an admin key holds widens that key's reach.
func TestGroupChangesAreAudited(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	rec := &recordedAudit{}
	svc.SetAuditRecorder(rec)
	seedWorkspace(t, st, "ws-a")
	seedWorkspace(t, st, "ws-b")
	ctx := WithVia(context.Background(), "dashboard")

	g, err := svc.CreateGroup(ctx, GroupInput{Name: "globex", Members: []string{"ws-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateGroup(ctx, g.ID, GroupUpdateInput{Members: []string{"ws-a", "ws-b"}, Patterns: []string{"github.com/globex/*"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range rec.entries {
		actions = append(actions, e.Action)
		if e.EntityType != "workspace_group" || e.EntityID != g.ID || e.Via != "dashboard" {
			t.Errorf("entry shape: %+v", e)
		}
	}
	if got := strings.Join(actions, ","); got != "group.create,group.update,group.delete" {
		t.Fatalf("actions: %s", got)
	}
	if n := rec.entries[0].Notes; n != "name=globex members=ws-a patterns=" {
		t.Fatalf("create note: %q", n)
	}
	was, now, _ := strings.Cut(rec.entries[1].Notes, "; now: ")
	if !strings.Contains(was, "members=ws-a patterns=") || !strings.Contains(now, "members=ws-a,ws-b patterns=github.com/globex/*") {
		t.Fatalf("update note must show the widened scope: %q", rec.entries[1].Notes)
	}
	if n := rec.entries[2].Notes; !strings.Contains(n, "name=globex") || !strings.Contains(n, "ws-b") {
		t.Fatalf("delete note must say what the group was: %q", n)
	}
}
