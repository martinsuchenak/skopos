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
	storage  *Storage
	now      func() time.Time
	onRevoke func(keyID string)
}

func NewService(storage *Storage) *Service {
	return &Service{storage: storage, now: time.Now}
}

// SetRevocationNotifier installs a callback invoked whenever a key actually
// transitions to revoked — wired to the event hub to terminate the key's
// open SSE streams.
func (s *Service) SetRevocationNotifier(fn func(keyID string)) { s.onRevoke = fn }

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

	secret, err := GenerateSecret()
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
// A successful transition fires the revocation notifier (the event hub
// terminates the key's open SSE streams).
func (s *Service) Revoke(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	revoked, err := s.storage.Revoke(ctx, id)
	if err != nil {
		return err
	}
	if revoked && s.onRevoke != nil {
		s.onRevoke(id)
	}
	return nil
}

// UpdateInput changes a key's name and/or scope. Nil fields are left
// unchanged (partial PATCH semantics).
type UpdateInput struct {
	Name       *string
	Workspaces []string // nil = unchanged; ["*"] or explicit ids otherwise
}

// Update edits a key's name and/or workspace scope. Revoked keys are frozen —
// re-mint instead. Scope changes take effect on the next request (lookups are
// not cached).
func (s *Service) Update(ctx context.Context, id string, input UpdateInput) (*Key, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	existing, err := s.storage.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.RevokedAt != nil {
		return nil, fmt.Errorf("%w: key is revoked; mint a new key instead", ErrInvalidInput)
	}

	name := existing.Name
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
		}
		if len(name) > 100 {
			return nil, fmt.Errorf("%w: name must be at most 100 characters", ErrInvalidInput)
		}
	}
	all := existing.AllWorkspaces
	workspaces := existing.Workspaces
	if input.Workspaces != nil {
		all = false
		workspaces = make([]string, 0, len(input.Workspaces))
		for _, w := range input.Workspaces {
			w = strings.TrimSpace(w)
			if w == "" {
				continue
			}
			if w == "*" {
				all = true
				continue
			}
			workspaces = append(workspaces, w)
		}
		if all && len(workspaces) > 0 {
			return nil, fmt.Errorf("%w: workspaces \"*\" cannot be combined with an explicit list", ErrInvalidInput)
		}
		if !all && len(workspaces) == 0 {
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
	}
	if err := s.storage.Update(ctx, id, name, all, workspaces); err != nil {
		return nil, err
	}
	updated, err := s.storage.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// Delete hard-deletes a key (audit trail included) — for cleaning up old or
// revoked keys. Active keys should usually be revoked first so their SSE
// streams terminate; hard-deleting an active key leaves its streams open
// until they reconnect.
func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.storage.Delete(ctx, id)
}

// GenerateSecret produces a key like "sk_" + 43 base64url chars (256 bits).
// Used for scoped keys and for suggesting a root key (skopos key
// generate-root) — the server treats whatever sits in auth.api_key as root.
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating key material: %w", err)
	}
	return "sk_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
