package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/inbox"
)

type noopInboxStore struct{}

func (s *noopInboxStore) CreateItem(context.Context, inbox.Item) error { return nil }
func (s *noopInboxStore) GetItem(_ context.Context, _ string) (*inbox.Item, error) {
	return nil, inbox.ErrNotFound
}
func (s *noopInboxStore) ListItems(context.Context, string, string, string, string) ([]inbox.Item, error) {
	return nil, nil
}
func (s *noopInboxStore) ItemsByPriority(context.Context, string, int) ([]inbox.Item, error) {
	return nil, nil
}
func (s *noopInboxStore) ItemWorkspace(_ context.Context, _ string) (string, error) {
	return "", inbox.ErrNotFound
}
func (s *noopInboxStore) PlanWorkspace(_ context.Context, _ string) (string, error) {
	return "", inbox.ErrNotFound
}
func (s *noopInboxStore) UpdateItem(context.Context, string, string, string, string, string, *int, time.Time) error {
	return nil
}
func (s *noopInboxStore) ReorderItems(context.Context, []string, time.Time) error { return nil }
func (s *noopInboxStore) RestoreItem(context.Context, string, time.Time) error { return nil }
func (s *noopInboxStore) ReopenItem(context.Context, string, time.Time) error { return nil }
func (s *noopInboxStore) ClaimItem(context.Context, string, string, time.Time) error {
	return nil
}
func (s *noopInboxStore) ReleaseItem(context.Context, string, time.Time) error { return nil }
func (s *noopInboxStore) ConvertItem(context.Context, string, string, time.Time) error {
	return nil
}
func (s *noopInboxStore) SetStatus(context.Context, string, inbox.Status, time.Time) error {
	return nil
}
func (s *noopInboxStore) CompleteForPlan(_ context.Context, _ string, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *noopInboxStore) DeleteItem(context.Context, string) error { return nil }
func (s *noopInboxStore) DeleteByFilter(_ context.Context, _ string, _ inbox.Status) (int64, error) {
	return 0, nil
}
func (s *noopInboxStore) RunInTx(ctx context.Context, fn func(inbox.Store) error) error {
	return fn(s)
}

// The two new inbox routes must coexist with the by-id/POST routes in the
// Go 1.22 mux — dispatch proves the patterns stay distinct.
func TestInboxCompleteAndPurgeRouteDispatch(t *testing.T) {
	mux := http.NewServeMux()
	registerInboxRoutes(mux, inbox.NewHandler(inbox.NewService(&noopInboxStore{}), testAuth("k")))

	r := httptest.NewRequest(http.MethodPost, "/api/inbox/item-1/complete", nil)
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	// 404 is the noop store's domain response (unknown item) — it proves
	// the route dispatched; only 405 would mean a routing miss.
	if w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("complete route did not dispatch: %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodDelete, "/api/inbox?workspace_id=ws-a&status=done", nil)
	r.Header.Set("Authorization", "Bearer k")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from purge route, got %d: %s", w.Code, w.Body.String())
	}
}
