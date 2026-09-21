package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
)

func TestRegisterBlackboardRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerBlackboardRoutes(mux, blackboard.NewHandler(
		blackboard.NewService(&noopBlackboardStore{}), testAuth(""),
	))
}

// The collection-level DELETE (bulk purge) must coexist with the by-id
// DELETE in the Go 1.22 mux — dispatch proves the patterns stay distinct.
func TestBlackboardPurgeRouteDispatch(t *testing.T) {
	mux := http.NewServeMux()
	registerBlackboardRoutes(mux, blackboard.NewHandler(
		blackboard.NewService(&noopBlackboardStore{}), testAuth("k"),
	))

	r := httptest.NewRequest(http.MethodDelete, "/api/blackboard/entries?workspace_id=ws-a&entry_type=bug", nil)
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from purge route, got %d: %s", w.Code, w.Body.String())
	}
}

type noopBlackboardStore struct{}

func (s *noopBlackboardStore) Write(_ context.Context, _ blackboard.Entry) error { return nil }
func (s *noopBlackboardStore) Bundle(_ context.Context, _, _, _ string) ([]blackboard.Entry, error) {
	return nil, nil
}
func (s *noopBlackboardStore) Promote(_ context.Context, _ string) error { return nil }
func (s *noopBlackboardStore) Delete(_ context.Context, _ string) error  { return nil }
func (s *noopBlackboardStore) DeleteByType(_ context.Context, _ string, _ blackboard.EntryType) (int64, error) {
	return 0, nil
}
func (s *noopBlackboardStore) Search(_ context.Context, _ blackboard.SearchFilters) ([]blackboard.Entry, error) {
	return nil, nil
}
func (s *noopBlackboardStore) SessionExists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (s *noopBlackboardStore) Get(_ context.Context, _ string) (*blackboard.Entry, error) {
	return nil, blackboard.ErrNotFound
}
