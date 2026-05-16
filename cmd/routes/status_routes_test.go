package routes

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/status"
)

func TestRegisterStatusRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerStatusRoutes(mux, status.NewHandler(status.NewService(&noopStore{}), ""))
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
func (s *noopStore) DeleteOldEvents(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *noopStore) DeleteOrphanedSessions(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
