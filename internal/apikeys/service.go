package apikeys

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
)

var (
	ErrInvalidInput = errors.New("invalid api keys input")
	ErrNotFound     = errors.New("not found")
)

type Service struct {
	storage *Storage
	now     func() time.Time
}

func NewService(storage *Storage) *Service {
	return &Service{storage: storage, now: time.Now}
}

type CreateInput struct {
	Name          string
	Workspaces    []string // exact ids, or a single "*" for all workspaces
	AllWorkspaces bool
}

// Create mints a key. The plaintext secret is returned once and never stored.
func (s *Service) Create(ctx context.Context, input CreateInput) (*CreateResult, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(input.Name) > 100 {
		return nil, fmt.Errorf("%w: name must be at most 100 characters", ErrInvalidInput)
	}

	workspaces := make([]string, 0, len(input.Workspaces))
	for _, w := range input.Workspaces {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if w == "*" {
			input.AllWorkspaces = true
			continue
		}
		workspaces = append(workspaces, w)
	}
	if input.AllWorkspaces && len(workspaces) > 0 {
		return nil, fmt.Errorf("%w: workspaces \"*\" cannot be combined with an explicit list", ErrInvalidInput)
	}
	if !input.AllWorkspaces && len(workspaces) == 0 {
		return nil, fmt.Errorf("%w: workspaces must be a list of workspace ids or \"*\"", ErrInvalidInput)
	}
	seen := map[string]bool{}
	unique := workspaces[:0]
	for _, w := range workspaces {
		if seen[w] {
			continue
		}
		seen[w] = true
		unique = append(unique, w)
	}
	workspaces = unique
	for _, w := range workspaces {
		exists, err := s.storage.WorkspaceExists(ctx, w)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("%w: workspace %s is not registered (register it first, e.g. via a session report or POST /api/workspaces)", ErrInvalidInput, w)
		}
	}

	secret, err := generateSecret()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	key := Key{
		ID:            ids.New(),
		Name:          input.Name,
		Prefix:        secret[:14],
		AllWorkspaces: input.AllWorkspaces,
		Workspaces:    workspaces,
		CreatedAt:     now,
	}
	if err := s.storage.Write(ctx, key, auth.HashKey(secret)); err != nil {
		return nil, err
	}
	return &CreateResult{Key: key, Secret: secret}, nil
}

// List returns every key including revoked ones (audit trail), newest first.
func (s *Service) List(ctx context.Context) ([]Key, error) {
	return s.storage.List(ctx)
}

// Revoke soft-deletes a key; revoking an already-revoked key is a no-op.
func (s *Service) Revoke(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.storage.Revoke(ctx, id)
}

// generateSecret produces a key like "sk_" + 43 base64url chars (256 bits).
func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating key material: %w", err)
	}
	return "sk_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
