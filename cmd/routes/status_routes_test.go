package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/status"
)



func TestRegisterStatusRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerStatusRoutes(mux, status.NewHandler(status.NewService(&noopStore{}), testAuth("")))
}

// The collection-level DELETE must coexist with DELETE /api/sessions/{id}
// in the Go 1.22 mux — dispatch proves the patterns stay distinct.
func TestStatusPurgeRouteDispatch(t *testing.T) {
	mux := http.NewServeMux()
	registerStatusRoutes(mux, status.NewHandler(status.NewService(&noopStore{}), testAuth("k")))

	r := httptest.NewRequest(http.MethodDelete, "/api/sessions", nil)
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from purge route, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"deleted":0}`+"\n" && w.Body.String() != `{"deleted":0}` {
		t.Fatalf("unexpected purge body: %q", w.Body.String())
	}
}

type noopStore struct{}

func (s *noopStore) RecordReport(ctx context.Context, report status.Event, sessionTitle string) error {
	return nil
}

func (s *noopStore) ListSessions(ctx context.Context, workspaceID string) ([]status.SessionSummary, error) {
	return nil, nil
}

func (s *noopStore) GetSession(ctx context.Context, id string) (*status.SessionDetail, error) {
	return nil, status.ErrNotFound
}

func (s *noopStore) ListEvents(ctx context.Context, sessionID string) ([]status.Event, error) {
	return nil, nil
}

func (s *noopStore) DeleteSession(_ context.Context, _ string) error { return nil }
func (s *noopStore) DeleteAllSessions(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (s *noopStore) ListActiveAgents(_ context.Context) ([]status.ActiveAgent, error) {
	return nil, nil
}
