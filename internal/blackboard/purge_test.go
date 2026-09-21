package blackboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func seedEntryRow(t *testing.T, s *Storage, id, ws, scope, branch, entryType string) {
	t.Helper()
	var branchVal any
	if branch != "" {
		branchVal = branch
	}
	if _, err := s.db.Exec(`INSERT INTO blackboard_entries (id, scope, workspace_id, branch_name, entry_type, title, content, author_agent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', 'agent-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		id, scope, ws, branchVal, entryType, id); err != nil {
		t.Fatal(err)
	}
}

func TestStorageDeleteByType(t *testing.T) {
	s := testStorage(t)
	seedEntryRow(t, s, "b1", "ws-a", "branch", "feat/x", "bug")
	seedEntryRow(t, s, "b2", "ws-a", "project", "", "bug")
	seedEntryRow(t, s, "b3", "ws-b", "branch", "feat/y", "bug")
	seedEntryRow(t, s, "f1", "ws-a", "branch", "feat/x", "finding")
	seedEntryRow(t, s, "b4", "ws-a", "session", "", "bug") // floating type, session scope

	n, err := s.DeleteByType(context.Background(), "ws-a", TypeBug)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("expected 3 bugs purged in ws-a (branch+project+session scopes), got %d", n)
	}
	remaining := map[string]bool{}
	rows, err := s.db.Query(`SELECT id FROM blackboard_entries`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining[id] = true
	}
	if !remaining["b3"] || !remaining["f1"] || len(remaining) != 2 {
		t.Fatalf("purge touched the wrong rows, remaining: %v", remaining)
	}

	// Idempotent: second run matches nothing, no error.
	if n, err = s.DeleteByType(context.Background(), "ws-a", TypeBug); err != nil || n != 0 {
		t.Fatalf("expected clean second purge, got n=%d err=%v", n, err)
	}
}

func TestServicePurgeTypeValidationAndScope(t *testing.T) {
	fake := &fakeStore{}
	svc := NewService(fake)
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k1", Workspaces: map[string]struct{}{"ws-a": {}}})

	if _, err := svc.PurgeType(ctx, "", TypeBug); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for missing workspace, got %v", err)
	}
	if _, err := svc.PurgeType(ctx, "ws-a", EntryType("incident")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unknown type, got %v", err)
	}
	if _, err := svc.PurgeType(ctx, "ws-b", TypeBug); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("expected ErrOutOfScope for foreign workspace, got %v", err)
	}
	if _, err := svc.PurgeType(ctx, " ws-a ", TypeBug); err != nil {
		t.Fatalf("scoped purge of own workspace failed: %v", err)
	}
}

func TestServicePurgeTypeFiltersByWorkspaceAndType(t *testing.T) {
	fake := &fakeStore{}
	fake.entries = []Entry{
		{ID: "1", WorkspaceID: "ws-a", EntryType: TypeBug},
		{ID: "2", WorkspaceID: "ws-a", EntryType: TypeBug},
		{ID: "3", WorkspaceID: "ws-a", EntryType: TypeFinding},
		{ID: "4", WorkspaceID: "ws-b", EntryType: TypeBug},
	}
	svc := NewService(fake)

	n, err := svc.PurgeType(context.Background(), "ws-a", TypeBug)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}
	if len(fake.entries) != 2 || fake.entries[0].ID != "3" || fake.entries[1].ID != "4" {
		t.Fatalf("wrong survivors: %+v", fake.entries)
	}
}

func TestHandlerPurgeType(t *testing.T) {
	h := NewHandler(NewService(&fakeStore{}), testAuth("k"))

	cases := []struct {
		name       string
		url        string
		principal  *auth.Principal
		wantStatus int
	}{
		{"missing params", "/api/blackboard/entries", nil, http.StatusBadRequest},
		{"missing type", "/api/blackboard/entries?workspace_id=ws-a", nil, http.StatusBadRequest},
		{"bad type", "/api/blackboard/entries?workspace_id=ws-a&entry_type=nope", nil, http.StatusBadRequest},
		{"out of scope", "/api/blackboard/entries?workspace_id=ws-b&entry_type=bug",
			&auth.Principal{KeyID: "k1", Workspaces: map[string]struct{}{"ws-a": {}}}, http.StatusForbidden},
		{"ok", "/api/blackboard/entries?workspace_id=ws-a&entry_type=bug", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodDelete, tc.url, nil)
			r.Header.Set("Authorization", "Bearer k")
			if tc.principal != nil {
				r = r.WithContext(auth.WithPrincipal(r.Context(), tc.principal))
			}
			w := httptest.NewRecorder()
			h.PurgeType(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				var out struct {
					Deleted int `json:"deleted"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
