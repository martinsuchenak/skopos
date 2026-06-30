package workspaces

import (
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

func (s *Service) Create(ctx context.Context, input CreateInput) (*Workspace, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	if input.ID == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	ws := Workspace{ID: input.ID, Name: input.Name, CreatedAt: s.now().UTC()}
	if err := s.store.Create(ctx, ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

func (s *Service) List(ctx context.Context) ([]Workspace, error) { return s.store.List(ctx) }

func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Delete(ctx, id)
}
