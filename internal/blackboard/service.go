package blackboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

func (s *Service) Write(ctx context.Context, input WriteInput) (*WriteResult, error) {
	input.Scope = Scope(strings.TrimSpace(string(input.Scope)))
	input.EntryType = EntryType(strings.TrimSpace(string(input.EntryType)))
	input.Title = strings.TrimSpace(input.Title)
	input.AuthorAgentID = strings.TrimSpace(input.AuthorAgentID)
	input.BranchName = strings.TrimSpace(input.BranchName)
	input.SessionID = strings.TrimSpace(input.SessionID)

	if input.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if input.AuthorAgentID == "" {
		return nil, fmt.Errorf("%w: author_agent_id is required", ErrInvalidInput)
	}
	if !validScope(input.Scope) {
		return nil, fmt.Errorf("%w: invalid scope %q", ErrInvalidInput, input.Scope)
	}
	if !validEntryType(input.EntryType) {
		return nil, fmt.Errorf("%w: invalid entry_type %q", ErrInvalidInput, input.EntryType)
	}
	if input.Scope == ScopeBranch && input.BranchName == "" {
		return nil, fmt.Errorf("%w: branch_name is required for branch scope", ErrInvalidInput)
	}
	if input.Scope == ScopeSession && input.SessionID == "" {
		return nil, fmt.Errorf("%w: session_id is required for session scope", ErrInvalidInput)
	}

	now := s.now().UTC()
	entry := Entry{
		ID:            generateID(),
		Scope:         input.Scope,
		BranchName:    input.BranchName,
		SessionID:     input.SessionID,
		EntryType:     input.EntryType,
		Title:         input.Title,
		Content:       strings.TrimSpace(input.Content),
		CodeRef:       strings.TrimSpace(input.CodeRef),
		AuthorAgentID: input.AuthorAgentID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.Write(ctx, entry); err != nil {
		return nil, err
	}
	return &WriteResult{ID: entry.ID, Scope: entry.Scope}, nil
}

func (s *Service) Bundle(ctx context.Context, branchName, sessionID string) (*Bundle, error) {
	entries, err := s.store.Bundle(ctx, strings.TrimSpace(branchName), strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	return &Bundle{
		Entries:        entries,
		MarkdownBundle: formatMarkdown(branchName, entries),
	}, nil
}

func (s *Service) Promote(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Promote(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Delete(ctx, id)
}

func validScope(s Scope) bool {
	switch s {
	case ScopeSession, ScopeBranch, ScopeProject:
		return true
	}
	return false
}

func validEntryType(t EntryType) bool {
	switch t {
	case TypeFinding, TypeDecision, TypeBug, TypeDebt, TypeWarning, TypeContext:
		return true
	}
	return false
}

func formatMarkdown(branchName string, entries []Entry) string {
	var sb strings.Builder
	sb.WriteString("## Skopos Knowledge Bundle\n")
	if branchName != "" {
		sb.WriteString(fmt.Sprintf("### Branch: %s\n\n", branchName))
	} else {
		sb.WriteString("\n")
	}

	if len(entries) == 0 {
		sb.WriteString("_No entries found._\n")
		return sb.String()
	}

	byType := make(map[EntryType][]Entry)
	for _, e := range entries {
		byType[e.EntryType] = append(byType[e.EntryType], e)
	}

	order := []EntryType{TypeBug, TypeDebt, TypeWarning, TypeFinding, TypeDecision, TypeContext}
	labels := map[EntryType]string{
		TypeBug:      "🐛 Bugs (cross-branch)",
		TypeDebt:     "⚠️ Tech Debt (cross-branch)",
		TypeWarning:  "⚠️ Warnings",
		TypeFinding:  "🔍 Findings",
		TypeDecision: "✅ Decisions",
		TypeContext:  "📋 Context",
	}

	for _, t := range order {
		es, ok := byType[t]
		if !ok {
			continue
		}
		sb.WriteString(fmt.Sprintf("#### %s\n", labels[t]))
		for _, e := range es {
			ref := ""
			if e.CodeRef != "" {
				ref = fmt.Sprintf(" (%s)", e.CodeRef)
			}
			sb.WriteString(fmt.Sprintf("- **%s**%s\n", e.Title, ref))
			if e.Content != "" {
				sb.WriteString(fmt.Sprintf("  %s\n", e.Content))
			}
			sb.WriteString(fmt.Sprintf("  _— %s_\n", e.AuthorAgentID))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func generateID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
