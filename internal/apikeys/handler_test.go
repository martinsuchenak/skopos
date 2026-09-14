package apikeys

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	logslog "github.com/paularlott/logger/slog"
)

func testHandler(t *testing.T) *Handler {
	t.Helper()
	st := testStorage(t)
	rest.SetLogger(logslog.New(logslog.Config{Level: "error"}))
	return NewHandler(NewService(st), workspaces.NewService(workspaces.NewStorage(st.db)))
}

// withPrincipal injects a principal the way the router middleware does.
func withPrincipal(r *http.Request, p *auth.Principal) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

var (
	rootPrin  = &auth.Principal{Root: true}
	scopedPrin = &auth.Principal{KeyID: "k1", Name: "ci", Workspaces: map[string]struct{}{"github.com/o/a": {}}}
)

func call(t *testing.T, h *Handler, method, target, body string, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if p != nil {
		req = withPrincipal(req, p)
	}
	w := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(target, "/api/keys/") && method == http.MethodDelete:
		req.SetPathValue("id", strings.TrimPrefix(target, "/api/keys/"))
		h.Revoke(w, req)
	case target == "/api/whoami":
		h.Whoami(w, req)
	case target == "/api/keys" && method == http.MethodPost:
		h.Create(w, req)
	case target == "/api/keys" && method == http.MethodGet:
		h.List(w, req)
	default:
		t.Fatalf("unrouted call: %s %s", method, target)
	}
	return w
}

func TestHandlerKeyManagementRequiresRoot(t *testing.T) {
	h := testHandler(t)

	for _, target := range []string{"/api/keys", "/api/whoami"} {
		if w := call(t, h, "GET", target, "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without principal: expected 401, got %d", target, w.Code)
		}
	}
	if w := call(t, h, "GET", "/api/keys", "", scopedPrin); w.Code != http.StatusForbidden {
		t.Fatalf("scoped key listing keys: expected 403, got %d %s", w.Code, w.Body.String())
	}
	if w := call(t, h, "POST", "/api/keys", `{"name":"x","workspaces":["*"]}`, scopedPrin); w.Code != http.StatusForbidden {
		t.Fatalf("scoped key creating key: expected 403, got %d", w.Code)
	}
}

func TestHandlerKeyLifecycle(t *testing.T) {
	h := testHandler(t)
	// seed a workspace for scoping
	if _, _, err := h.registry.Create(context.Background(), workspaces.CreateInput{ID: "github.com/o/a"}); err != nil {
		t.Fatal(err)
	}

	w := call(t, h, "POST", "/api/keys", `{"name":"ci","workspaces":["github.com/o/a"]}`, rootPrin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Key struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
		} `json:"key"`
		Secret string `json:"key_secret"`
	}
	json.NewDecoder(w.Body).Decode(&created)
	if created.Key.ID == "" || !strings.HasPrefix(created.Secret, "sk_") {
		t.Fatalf("bad create response: %s", w.Body.String())
	}

	wl := call(t, h, "GET", "/api/keys", "", rootPrin)
	if wl.Code != http.StatusOK {
		t.Fatalf("list: %d %s", wl.Code, wl.Body.String())
	}
	if !strings.Contains(wl.Body.String(), `"ci"`) {
		t.Fatalf("list must contain the key name: %d %s", wl.Code, wl.Body.String())
	}

	if w := call(t, h, "DELETE", "/api/keys/"+created.Key.ID, "", rootPrin); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, h, "DELETE", "/api/keys/missing", "", rootPrin); w.Code != http.StatusNotFound {
		t.Fatalf("revoke missing: expected 404, got %d", w.Code)
	}
}

func TestHandlerWhoami(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	for _, id := range []string{"github.com/o/a", "github.com/o/b"} {
		if _, _, err := h.registry.Create(ctx, workspaces.CreateInput{ID: id}); err != nil {
			t.Fatal(err)
		}
	}

	w := call(t, h, "GET", "/api/whoami", "", scopedPrin)
	if w.Code != http.StatusOK {
		t.Fatalf("whoami: %d", w.Code)
	}
	var resp struct {
		Root       bool `json:"root"`
		Key        struct {
			AllWorkspaces bool     `json:"all_workspaces"`
			Workspaces    []string `json:"workspaces"`
		} `json:"key"`
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Root {
		t.Fatal("scoped principal must not report root")
	}
	if len(resp.Workspaces) != 1 || resp.Workspaces[0].ID != "github.com/o/a" {
		t.Fatalf("whoami workspaces must be the registry ∩ scope, got %+v", resp.Workspaces)
	}

	w = call(t, h, "GET", "/api/whoami", "", rootPrin)
	if !strings.Contains(w.Body.String(), `"root":true`) {
		t.Fatalf("root whoami must report root:true: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"key":`) {
		t.Fatalf("root whoami must not carry a key block: %s", w.Body.String())
	}
	if got := strings.Count(w.Body.String(), `"id":"github.com/o/`); got != 2 {
		t.Fatalf("root whoami must see both workspaces, got %d: %s", got, w.Body.String())
	}
}
