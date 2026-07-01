package routes

import (
	"context"
	"net/http"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
)

func TestRegisterBlackboardRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerBlackboardRoutes(mux, blackboard.NewHandler(
		blackboard.NewService(&noopBlackboardStore{}), "",
	))
}

type noopBlackboardStore struct{}

func (s *noopBlackboardStore) Write(_ context.Context, _ blackboard.Entry) error { return nil }
func (s *noopBlackboardStore) Bundle(_ context.Context, _, _, _ string) ([]blackboard.Entry, error) {
	return nil, nil
}
func (s *noopBlackboardStore) Promote(_ context.Context, _ string) error { return nil }
func (s *noopBlackboardStore) Delete(_ context.Context, _ string) error  { return nil }
func (s *noopBlackboardStore) Search(_ context.Context, _ blackboard.SearchFilters) ([]blackboard.Entry, error) {
	return nil, nil
}
func (s *noopBlackboardStore) DeleteBySession(_ context.Context, _ string) error { return nil }
func (s *noopBlackboardStore) SessionExists(_ context.Context, _ string) (bool, error) { return false, nil }
func (s *noopBlackboardStore) Get(_ context.Context, _ string) (*blackboard.Entry, error) {
	return nil, blackboard.ErrNotFound
}
