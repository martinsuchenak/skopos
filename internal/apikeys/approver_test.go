package apikeys

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	logslog "github.com/paularlott/logger/slog"
)

// RequireApprover is the gate the five human-only inbox actions will enforce
// (item 3 wires the actions; this covers the permission axis itself).
// Matrix: internal callers pass like RequireRoot, root passes implicitly,
// approver keys pass, every other scoped key is rejected.
func TestRequireApproverMatrix(t *testing.T) {
	ctxWith := func(p *auth.Principal) context.Context {
		return auth.WithPrincipal(context.Background(), p)
	}
	scoped := &auth.Principal{KeyID: "k1", Workspaces: map[string]struct{}{"ws": {}}}
	approver := &auth.Principal{KeyID: "k2", Approver: true, Workspaces: map[string]struct{}{"ws": {}}}

	if err := auth.RequireApprover(context.Background()); err != nil {
		t.Fatalf("internal caller must pass, got %v", err)
	}
	if err := auth.RequireApprover(ctxWith(&auth.Principal{Root: true})); err != nil {
		t.Fatalf("root must pass, got %v", err)
	}
	if err := auth.RequireApprover(ctxWith(approver)); err != nil {
		t.Fatalf("approver key must pass, got %v", err)
	}
	if err := auth.RequireApprover(ctxWith(scoped)); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("plain scoped key must be rejected, got %v", err)
	}
}

func TestApproverGrantLifecycle(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")

	// Granted at creation; the flag travels through the full auth path:
	// storage lookup → KeyInfo → Authenticate → Principal.
	result, err := svc.Create(ctx, CreateInput{Name: "relay", Workspaces: []string{"ws-a"}, Approver: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Key.Approver {
		t.Fatal("key must carry the approver flag")
	}

	authn := auth.NewAuthenticator("root-secret", st)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+result.Secret)
	p := authn.Authenticate(req)
	if p == nil {
		t.Fatal("approver key must authenticate")
	}
	if !p.Approver {
		t.Fatal("Authenticate must carry Approver onto the Principal")
	}

	// The root key keeps implicit approval rights.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer root-secret")
	if p := authn.Authenticate(req); p == nil || !p.Root {
		t.Fatal("root key must authenticate as root")
	}

	// The flag survives unrelated edits (nil = unchanged), is revocable, and
	// dies with the key.
	name := "relay2"
	updated, err := svc.Update(ctx, result.Key.ID, UpdateInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Approver {
		t.Fatal("approver flag must survive a name edit")
	}
	no := false
	if updated, err = svc.Update(ctx, result.Key.ID, UpdateInput{Approver: &no}); err != nil {
		t.Fatal(err)
	}
	if updated.Approver {
		t.Fatal("approver flag must be revocable")
	}
	yes := true
	if _, err = svc.Update(ctx, result.Key.ID, UpdateInput{Approver: &yes}); err != nil {
		t.Fatal(err)
	}
	if err = svc.Revoke(ctx, result.Key.ID); err != nil {
		t.Fatal(err)
	}
	if p := authn.Authenticate(bearerRequest(result.Secret)); p != nil {
		t.Fatal("revoked approver key must not authenticate")
	}
}

func bearerRequest(secret string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	return req
}

// The REST surface accepts and reports the flag; whoami shows it.
func TestApproverHandlerSurface(t *testing.T) {
	st := testStorage(t)
	rest.SetLogger(logslog.New(logslog.Config{Level: "error"}))
	h := NewHandler(NewService(st), workspaces.NewService(workspaces.NewStorage(st.db)))
	seedWorkspace(t, st, "github.com/o/a")

	w := call(t, h, "POST", "/api/keys",
		`{"name":"relay","workspaces":["github.com/o/a"],"approver":true}`, rootPrin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create approver key: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"approver":true`) {
		t.Fatalf("create response must carry the flag: %s", w.Body.String())
	}
	var created struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// whoami for the approver principal reports the permission.
	req := httptest.NewRequest("GET", "/api/whoami", nil)
	req = withPrincipal(req, &auth.Principal{
		KeyID: created.Key.ID, Name: "relay", Approver: true,
		Workspaces: map[string]struct{}{"github.com/o/a": {}},
	})
	rec := httptest.NewRecorder()
	h.Whoami(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approver":true`) {
		t.Fatalf("whoami must report approver: %d %s", rec.Code, rec.Body.String())
	}
}
