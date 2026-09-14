package workspaces

import (

	"github.com/martinsuchenak/skopos/internal/auth"
	"context"
	"fmt"
	"strings"
	"time"
)

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service { return &Service{store: store, now: time.Now} }

func (s *Service) Create(ctx context.Context, input CreateInput) (*Workspace, bool, error) {
	// The registry and git_url feed the server-side clone path and define
	// the scoping graph itself: workspace management is root-only.
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, false, err
	}
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	if input.ID == "" {
		return nil, false, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	ws := Workspace{ID: input.ID, Name: input.Name, GitURL: strings.TrimSpace(input.GitURL), CreatedAt: s.now().UTC()}
	created, err := s.store.Create(ctx, ws)
	if err != nil {
		return nil, false, err
	}
	return &ws, created, nil
}

func (s *Service) List(ctx context.Context) ([]Workspace, error) {
	all, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	// Scoped keys see exactly their slice of the registry.
	if !auth.ScopedContext(ctx) {
		return all, nil
	}
	out := make([]Workspace, 0, len(all))
	for _, ws := range all {
		if auth.PrincipalFromContext(ctx).CanAccess(ws.ID) {
			out = append(out, ws)
		}
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Workspace, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	if err := auth.RequireRoot(ctx); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Delete(ctx, id)
}
