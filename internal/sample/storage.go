package sample

import (
	"context"
	"fmt"
	"sync"
)

type Storage struct {
	mu    sync.RWMutex
	items map[string]*Sample
}

func NewStorage() *Storage {
	return &Storage{
		items: make(map[string]*Sample),
	}
}

func (s *Storage) List(ctx context.Context) ([]Sample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Sample, 0, len(s.items))
	for _, item := range s.items {
		result = append(result, *item)
	}
	return result, nil
}

func (s *Storage) Get(ctx context.Context, id string) (*Sample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.items[id]
	if !ok {
		return nil, fmt.Errorf("sample not found: %s", id)
	}
	return item, nil
}

func (s *Storage) Create(ctx context.Context, sample *Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items[sample.ID] = sample
	return nil
}
