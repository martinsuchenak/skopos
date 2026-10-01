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

// --- storage: LookupKey resolution matrix (docs/design/agent-pipeline.md §3) ---

func seedGroup(t *testing.T, s *Storage, id, name string, members, patterns []string) {
	t.Helper()
	now := formatTime(timeNowUTC())
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO workspace_groups (id, name, description, created_at) VALUES (?, ?, '', ?)`, id, name, now); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO workspace_group_members (group_id, workspace_id) VALUES (?, ?)`, id, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range patterns {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO workspace_group_patterns (group_id, pattern) VALUES (?, ?)`, id, p); err != nil {
			t.Fatal(err)
		}
	}
}

func seedKey(t *testing.T, s *Storage, id, hash string, all bool, workspacesList, groups []string) {
	t.Helper()
	allInt := 0
	if all {
		allInt = 1
	}
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, created_at)
		 VALUES (?, ?, ?, 'sk_x', ?, ?)`, id, id, hash, allInt, formatTime(timeNowUTC())); err != nil {
		t.Fatal(err)
	}
	for _, w := range workspacesList {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO api_key_workspaces (api_key_id, workspace_id) VALUES (?, ?)`, id, w); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range groups {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO api_key_groups (api_key_id, group_id) VALUES (?, ?)`, id, g); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLookupKeyGroupResolution(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()

	seedWorkspace(t, s, "github.com/fortix/freedom3")
	seedWorkspace(t, s, "github.com/fortix/portal")
	seedWorkspace(t, s, "github.com/other/repo")
	// Registered later to prove pattern picks up new workspaces on the next
	// lookup (unregistered ids never match).
	seedGroup(t, s, "g1", "work", []string{"github.com/other/repo"}, []string{"github.com/fortix/*"})
	seedKey(t, s, "k-explicit", "h1", false, []string{"github.com/fortix/portal"}, nil)
	seedKey(t, s, "k-groups", "h2", false, nil, []string{"g1"})
	seedKey(t, s, "k-both", "h3", false, []string{"github.com/fortix/portal"}, []string{"g1"})
	seedKey(t, s, "k-all", "h4", true, nil, []string{"g1"})
	seedKey(t, s, "k-revoked", "h5", false, nil, []string{"g1"})
	s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = 'k-revoked'`, formatTime(timeNowUTC()))

	// Explicit only.
	info, err := s.LookupKey(ctx, "h1")
	if err != nil || info == nil {
		t.Fatalf("explicit lookup failed: %v %v", info, err)
	}
	if len(info.Workspaces) != 1 || info.Workspaces[0] != "github.com/fortix/portal" {
		t.Fatalf("explicit scope: got %v", info.Workspaces)
	}

	// Group members + pattern matches over registered workspaces.
	info, _ = s.LookupKey(ctx, "h2")
	got := strings.Join(info.Workspaces, ",")
	want := "github.com/fortix/freedom3,github.com/fortix/portal,github.com/other/repo"
	if got != want {
		t.Fatalf("group scope: got %q want %q", got, want)
	}

	// Explicit + group union, deduped.
	info, _ = s.LookupKey(ctx, "h3")
	if len(info.Workspaces) != 3 {
		t.Fatalf("explicit+group union: got %v", info.Workspaces)
	}

	// "*" ignores groups entirely.
	info, _ = s.LookupKey(ctx, "h4")
	if !info.AllWorkspaces || len(info.Workspaces) != 0 {
		t.Fatalf("all-workspaces key must not carry resolved rows: %+v", info)
	}

	// Revoked keys with groups resolve to nothing, like revoked explicit keys.
	if info, _ := s.LookupKey(ctx, "h5"); info != nil {
		t.Fatal("revoked key must not resolve")
	}

	// A newly registered workspace matching the pattern joins the scope on
	// the next lookup — no cache to clear.
	seedWorkspace(t, s, "github.com/fortix/newrepo")
	info, _ = s.LookupKey(ctx, "h2")
	found := false
	for _, w := range info.Workspaces {
		if w == "github.com/fortix/newrepo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("newly registered pattern match missing from scope: %v", info.Workspaces)
	}
}

func TestPatternSegmentSemantics(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	// `*` never crosses `/`: a repo two levels deep must not match.
	seedWorkspace(t, s, "github.com/fortix/freedom3")
	seedWorkspace(t, s, "github.com/fortix/team/sub/repo")
	seedGroup(t, s, "g1", "seg", nil, []string{"github.com/fortix/*"})
	seedKey(t, s, "k1", "h1", false, nil, []string{"g1"})

	info, err := s.LookupKey(ctx, "h1")
	if err != nil || info == nil {
		t.Fatalf("lookup failed: %v %v", info, err)
	}
	if len(info.Workspaces) != 1 || info.Workspaces[0] != "github.com/fortix/freedom3" {
		t.Fatalf("pattern must match one segment only: %v", info.Workspaces)
	}
}

// --- service: validation, key groups, notifiers ---

func scopedCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k-scoped", Name: "scoped",
		Workspaces: map[string]struct{}{"github.com/o/a": {}},
	})
}

func TestGroupValidation(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")

	for _, tc := range []struct {
		name  string
		input GroupInput
		want  error
	}{
		{"missing name", GroupInput{Members: []string{"ws-a"}}, ErrInvalidInput},
		{"no members or patterns", GroupInput{Name: "g"}, ErrInvalidInput},
		{"unknown member", GroupInput{Name: "g", Members: []string{"nope"}}, ErrInvalidInput},
		{"double-star pattern", GroupInput{Name: "g", Patterns: []string{"github.com/**"}}, ErrInvalidInput},
		{"malformed pattern", GroupInput{Name: "g", Patterns: []string{"github.com/["}}, ErrInvalidInput},
	} {
		if _, err := svc.CreateGroup(ctx, tc.input); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}

	// Duplicate names are a conflict, not an invalid input.
	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "work", Members: []string{"ws-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "work", Patterns: []string{"x/*"}}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("duplicate name: expected ErrDuplicateName, got %v", err)
	}

	// Group management is root-only: a scoped key cannot create, list, or
	// mutate groups even though the handler also gates.
	if _, err := svc.CreateGroup(scopedCtx(), GroupInput{Name: "sneaky", Members: []string{"ws-a"}}); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("scoped create group: %v", err)
	}
	if _, err := svc.ListGroups(scopedCtx()); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("scoped list groups: %v", err)
	}
	if _, err := svc.WhoCan(scopedCtx(), "ws-a"); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("scoped who-can: %v", err)
	}
}

func TestKeyWithGroups(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")
	group, err := svc.CreateGroup(ctx, GroupInput{Name: "work", Members: []string{"ws-a"}})
	if err != nil {
		t.Fatal(err)
	}

	// Create by group NAME; unknown groups are rejected; groups cannot
	// combine with "*".
	if _, err := svc.Create(ctx, CreateInput{Name: "bad", Groups: []string{"nope"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown group: %v", err)
	}
	if _, err := svc.Create(ctx, CreateInput{Name: "bad", Workspaces: []string{"*"}, Groups: []string{"work"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("groups with star: %v", err)
	}
	result, err := svc.Create(ctx, CreateInput{Name: "work-key", Groups: []string{"work"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Key.Groups) != 1 || result.Key.Groups[0] != "work" {
		t.Fatalf("created key groups: %v", result.Key.Groups)
	}

	// By group ID as well (refs accept either).
	if _, err := svc.Create(ctx, CreateInput{Name: "id-key", Groups: []string{group.ID}}); err != nil {
		t.Fatalf("create by group id: %v", err)
	}

	// The key resolves to the group's members.
	st.db.ExecContext(ctx, `UPDATE api_keys SET key_hash = 'lookup-hash' WHERE id = ?`, result.Key.ID)
	info, err := st.LookupKey(ctx, "lookup-hash")
	if err != nil || info == nil {
		t.Fatalf("lookup: %v %v", info, err)
	}
	if len(info.Workspaces) != 1 || info.Workspaces[0] != "ws-a" {
		t.Fatalf("group-resolved scope: %v", info.Workspaces)
	}

	// Editing groups replaces the list; clearing explicit workspaces is
	// legal while groups carry the scope.
	updated, err := svc.Update(ctx, result.Key.ID, UpdateInput{Workspaces: []string{}})
	if err != nil {
		t.Fatalf("clear workspaces with groups kept: %v", err)
	}
	if len(updated.Groups) != 1 {
		t.Fatalf("groups must survive a workspace edit: %v", updated.Groups)
	}
	if _, err := svc.Update(ctx, result.Key.ID, UpdateInput{Groups: []string{}, Workspaces: []string{}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("clearing all scope must fail: %v", err)
	}

	// Deleting the group narrows the key back to nothing group-derived.
	if err := svc.DeleteGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	after, err := st.LookupKey(ctx, "lookup-hash")
	if err != nil || after == nil {
		t.Fatalf("lookup after group delete: %v %v", after, err)
	}
	if len(after.Workspaces) != 0 {
		t.Fatalf("group delete must narrow scope, got %v", after.Workspaces)
	}
}

func TestScopeNotifierTriggers(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "github.com/fortix/freedom3")

	var dropped []string
	svc.SetScopeNotifier(func(keyIDs []string) { dropped = append(dropped, keyIDs...) })

	group, err := svc.CreateGroup(ctx, GroupInput{Name: "work", Patterns: []string{"github.com/fortix/*"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Create(ctx, CreateInput{Name: "k", Groups: []string{"work"}})
	if err != nil {
		t.Fatal(err)
	}

	// A group membership change re-scopes every key holding the group.
	dropped = nil
	if _, err := svc.UpdateGroup(ctx, group.ID, GroupUpdateInput{Members: []string{"github.com/fortix/freedom3"}}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != result.Key.ID {
		t.Fatalf("group update must drop holding keys, got %v", dropped)
	}

	// A key scope edit drops that key.
	dropped = nil
	if _, err := svc.Update(ctx, result.Key.ID, UpdateInput{Workspaces: []string{"github.com/fortix/freedom3"}}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != result.Key.ID {
		t.Fatalf("key scope edit must drop the key, got %v", dropped)
	}

	// Name-only edits drop nothing.
	dropped = nil
	newName := "k2"
	if _, err := svc.Update(ctx, result.Key.ID, UpdateInput{Name: &newName}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("name edit must not drop streams, got %v", dropped)
	}

	// A newly registered workspace matching a pattern drops holding keys.
	dropped = nil
	svc.OnWorkspaceRegistered(ctx, "github.com/fortix/portal")
	if len(dropped) != 1 || dropped[0] != result.Key.ID {
		t.Fatalf("workspace registration must drop pattern-matched keys, got %v", dropped)
	}
	// Non-matching registrations drop nothing.
	dropped = nil
	svc.OnWorkspaceRegistered(ctx, "github.com/other/repo")
	if len(dropped) != 0 {
		t.Fatalf("unrelated registration must not drop keys, got %v", dropped)
	}
}

func TestWhoCan(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")
	seedWorkspace(t, st, "ws-b")

	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "membered", Members: []string{"ws-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "patterned", Patterns: []string{"ws-*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateInput{Name: "explicit-key", Workspaces: []string{"ws-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateInput{Name: "member-key", Groups: []string{"membered"}}); err != nil {
		t.Fatal(err)
	}
	// Holds both groups; ws-a is reached via member (closer than pattern).
	if _, err := svc.Create(ctx, CreateInput{Name: "pattern-key", Groups: []string{"patterned", "membered"}}); err != nil {
		t.Fatal(err)
	}

	reach, err := svc.WhoCan(ctx, "ws-a")
	if err != nil {
		t.Fatal(err)
	}
	via := map[string]string{}
	for _, r := range reach {
		via[r.Key.Name] = r.Via
	}
	if via["explicit-key"] != "explicit" {
		t.Fatalf("explicit-key via: %q", via["explicit-key"])
	}
	if via["member-key"] != "group:membered" {
		t.Fatalf("member-key via: %q", via["member-key"])
	}
	if via["pattern-key"] != "group:membered" {
		t.Fatalf("member beats pattern: %q", via["pattern-key"])
	}

	reach, err = svc.WhoCan(ctx, "ws-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(reach) != 1 || reach[0].Key.Name != "pattern-key" || reach[0].Via != "pattern:patterned:ws-*" {
		t.Fatalf("ws-b reach: %+v", reach)
	}

	if _, err := svc.WhoCan(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty workspace: %v", err)
	}
}

// --- handler: root-only group API + whoami enrichment ---

func groupCall(t *testing.T, h *Handler, method, target, body string, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	if strings.HasPrefix(target, "/api/groups/") {
		req.SetPathValue("id", strings.Trim(strings.TrimPrefix(target, "/api/groups/"), "/"))
	}
	w := httptest.NewRecorder()
	switch {
	case target == "/api/groups" && method == http.MethodPost:
		h.CreateGroup(w, req)
	case target == "/api/groups" && method == http.MethodGet:
		h.ListGroups(w, req)
	case strings.HasPrefix(target, "/api/groups/") && method == http.MethodGet:
		h.GetGroup(w, req)
	case strings.HasPrefix(target, "/api/groups/") && method == http.MethodPatch:
		h.UpdateGroup(w, req)
	case strings.HasPrefix(target, "/api/groups/") && method == http.MethodDelete:
		h.DeleteGroup(w, req)
	case strings.HasPrefix(target, "/api/keys/who-can") && method == http.MethodGet:
		h.WhoCan(w, req)
	default:
		t.Fatalf("unrouted group call: %s %s", method, target)
	}
	return w
}

func TestHandlerGroupsRequireRoot(t *testing.T) {
	h := testHandler(t)

	if w := groupCall(t, h, "GET", "/api/groups", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d", w.Code)
	}
	for _, target := range []string{"/api/groups", "/api/keys/who-can?workspace=x"} {
		if w := groupCall(t, h, "GET", target, "", scopedPrin); w.Code != http.StatusForbidden {
			t.Fatalf("scoped principal on %s: expected 403, got %d %s", target, w.Code, w.Body.String())
		}
	}
	if w := groupCall(t, h, "POST", "/api/groups", `{"name":"x"}`, scopedPrin); w.Code != http.StatusForbidden {
		t.Fatalf("scoped create: expected 403, got %d", w.Code)
	}
}

func TestHandlerGroupLifecycleAndWhoCan(t *testing.T) {
	h := testHandler(t)

	w := groupCall(t, h, "POST", "/api/groups",
		`{"name":"work","patterns":["github.com/fortix/*"]}`, rootPrin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", w.Code, w.Body.String())
	}
	var group Group
	if err := json.Unmarshal(w.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}

	if w := groupCall(t, h, "POST", "/api/groups", `{"name":"work","patterns":["other/*"]}`, rootPrin); w.Code != http.StatusConflict {
		t.Fatalf("duplicate name: expected 409, got %d", w.Code)
	}

	w = groupCall(t, h, "GET", "/api/groups", "", rootPrin)
	if w.Code != http.StatusOK {
		t.Fatalf("list groups: %d", w.Code)
	}

	// A key holding the group, minted through the REST surface.
	w = call(t, h, "POST", "/api/keys", `{"name":"work-key","workspaces":[],"groups":["work"]}`, rootPrin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create key with groups: %d %s", w.Code, w.Body.String())
	}

	whoCan := groupCall(t, h, "GET", "/api/keys/who-can?workspace=github.com/fortix/freedom3", "", rootPrin)
	if whoCan.Code != http.StatusOK {
		t.Fatalf("who-can: %d %s", whoCan.Code, whoCan.Body.String())
	}
	if !strings.Contains(whoCan.Body.String(), "pattern:work:github.com/fortix/*") {
		t.Fatalf("who-can must explain pattern reach: %s", whoCan.Body.String())
	}

	if w := groupCall(t, h, "PATCH", "/api/groups/"+group.ID,
		`{"name":"work2"}`, rootPrin); w.Code != http.StatusOK {
		t.Fatalf("patch group: %d %s", w.Code, w.Body.String())
	}
	if w := groupCall(t, h, "DELETE", "/api/groups/"+group.ID, "", rootPrin); w.Code != http.StatusNoContent {
		t.Fatalf("delete group: %d", w.Code)
	}
	if w := groupCall(t, h, "GET", "/api/groups/"+group.ID, "", rootPrin); w.Code != http.StatusNotFound {
		t.Fatalf("get deleted group: expected 404, got %d", w.Code)
	}
}

func TestHandlerWhoamiWithGroups(t *testing.T) {
	st := testStorage(t)
	rest.SetLogger(logslog.New(logslog.Config{Level: "error"}))
	h := NewHandler(NewService(st), workspaces.NewService(workspaces.NewStorage(st.db)))
	seedWorkspace(t, st, "github.com/o/a")

	if w := groupCall(t, h, "POST", "/api/groups", `{"name":"work","members":["github.com/o/a"]}`, rootPrin); w.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", w.Code, w.Body.String())
	}
	createResp := call(t, h, "POST", "/api/keys", `{"name":"wg-key","workspaces":[],"groups":["work"]}`, rootPrin)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// The key's principal carries only the resolved group scope; whoami
	// explains each reachable workspace with its group and via.
	info, err := st.LookupKey(context.Background(), func() string {
		row, err := st.db.QueryContext(context.Background(),
			`SELECT key_hash FROM api_keys WHERE id = ?`, created.Key.ID)
		if err != nil || !row.Next() {
			t.Fatal("key hash lookup failed")
		}
		defer row.Close()
		var hash string
		_ = row.Scan(&hash)
		return hash
	}())
	if err != nil || info == nil {
		t.Fatalf("lookup: %v %v", info, err)
	}
	prin := &auth.Principal{KeyID: info.ID, Name: info.Name, Workspaces: map[string]struct{}{}}
	for _, w := range info.Workspaces {
		prin.Workspaces[w] = struct{}{}
	}

	req := httptest.NewRequest("GET", "/api/whoami", nil)
	req = withPrincipal(req, prin)
	w := httptest.NewRecorder()
	h.Whoami(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("whoami: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"groups":[{`) || !strings.Contains(body, `"via":"group:work"`) {
		t.Fatalf("whoami must include groups and via, got: %s", body)
	}
}

// The workspaces create-notifier fires the pattern trigger end to end:
// registering a workspace that matches a group pattern drops the holding
// keys' streams (wired in serve.go; verified here at service level).
func TestWorkspaceRegistrationTriggersScopeDrop(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()

	var dropped []string
	svc.SetScopeNotifier(func(keyIDs []string) { dropped = append(dropped, keyIDs...) })

	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "work", Patterns: []string{"github.com/fortix/*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateInput{Name: "k", Groups: []string{"work"}}); err != nil {
		t.Fatal(err)
	}

	registry := workspaces.NewService(workspaces.NewStorage(st.db))
	registry.SetCreateNotifier(func(wsID string) { svc.OnWorkspaceRegistered(context.Background(), wsID) })
	if _, _, err := registry.Create(ctx, workspaces.CreateInput{ID: "github.com/fortix/portal"}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) == 0 {
		t.Fatal("workspace registration must fire the scope notifier for pattern-matched keys")
	}

	// Idempotent registration (upsert) must not re-drop.
	dropped = nil
	if _, _, err := registry.Create(ctx, workspaces.CreateInput{ID: "github.com/fortix/portal"}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("re-registering an existing workspace must not drop: %v", dropped)
	}
}
