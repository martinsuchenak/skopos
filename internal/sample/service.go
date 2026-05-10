package sample

import (
	"context"
)

type Sample struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type CreateSampleInput struct {
	Name string `json:"name"`
}

type Service struct {
	storage *Storage
}

func NewService(s *Storage) *Service {
	return &Service{storage: s}
}

func (s *Service) List(ctx context.Context) ([]Sample, error) {
	return s.storage.List(ctx)
}

func (s *Service) Get(ctx context.Context, id string) (*Sample, error) {
	return s.storage.Get(ctx, id)
}

func (s *Service) Create(ctx context.Context, input CreateSampleInput) (*Sample, error) {
	sample := Sample{
		ID:   generateID(),
		Name: input.Name,
	}
	if err := s.storage.Create(ctx, &sample); err != nil {
		return nil, err
	}
	return &sample, nil
}

func generateID() string {
	// TODO: Use UUIDv7 generation
	return "sample-id"
}
