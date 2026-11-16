package inbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// Starting an item from a template (tmp/templates-schedules — design.md,
// part 2). The templates are held by the pipeline settings; the inbox reads
// the one in force for a workspace through TemplateSource.

// ItemTemplate is a template as an item is started from it.
type ItemTemplate struct {
	Name, Rev       string
	Title, Brief    string
	Tags, Needs     []string
	Agents          string
	AutoApprovePlan bool
}

// TemplateSource returns the template a workspace holds of a name (its own,
// else its group's); an error wrapping ErrTemplateNotFound when none.
type TemplateSource func(ctx context.Context, ws, name string) (*ItemTemplate, error)

// ErrTemplateNotFound: the workspace holds no template of that name.
var ErrTemplateNotFound = errors.New("no such template")

// SetTemplates wires the template source.
func (s *Service) SetTemplates(fn TemplateSource) { s.templates = fn }

// expandTemplate fills a template's placeholders for a workspace at a time:
// {date} 2026-03-04, {month} 2026-03, {year} 2026, {workspace} the
// repository's last path element. Other braces stay as they are.
func expandTemplate(text, ws string, at time.Time) string {
	short := ws
	if i := strings.LastIndex(ws, "/"); i >= 0 {
		short = ws[i+1:]
	}
	at = at.UTC()
	return strings.NewReplacer(
		"{date}", at.Format("2006-01-02"),
		"{month}", at.Format("2006-01"),
		"{year}", at.Format("2006"),
		"{workspace}", short,
	).Replace(text)
}

// applyTemplate fills what the request left empty from the named template.
// A template that approves plans automatically hands over the approver's
// gate: starting from it needs the approver permission, as switching it on
// does.
func (s *Service) applyTemplate(ctx context.Context, in *CreateInput) (*ItemTemplate, error) {
	name := strings.TrimSpace(in.Template)
	if name == "" {
		if strings.TrimSpace(in.Extra) != "" {
			return nil, fmt.Errorf("%w: extra goes with a template", ErrInvalidInput)
		}
		return nil, nil
	}
	if in.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: a template is held for a workspace: give workspace_id", ErrInvalidInput)
	}
	if s.templates == nil {
		return nil, fmt.Errorf("%w: templates are not available on this server", ErrInvalidInput)
	}
	t, err := s.templates(ctx, in.WorkspaceID, name)
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			return nil, fmt.Errorf("%w: workspace %s has no template %q", ErrInvalidInput, in.WorkspaceID, name)
		}
		return nil, err
	}
	now := s.now()
	if strings.TrimSpace(in.Title) == "" {
		in.Title = expandTemplate(t.Title, in.WorkspaceID, now)
	}
	if strings.TrimSpace(in.Content) == "" {
		in.Content = expandTemplate(t.Brief, in.WorkspaceID, now)
	}
	if extra := strings.TrimSpace(in.Extra); extra != "" {
		in.Content = strings.TrimSpace(in.Content + "\n\n" + extra)
	}
	if len(in.Tags) == 0 {
		in.Tags = t.Tags
	}
	if len(in.Needs) == 0 {
		in.Needs = t.Needs
	}
	if in.Agents == "" {
		in.Agents = t.Agents
	}
	if t.AutoApprovePlan {
		if err := auth.RequireApprover(ctx); err != nil {
			return nil, fmt.Errorf("%w: template %s approves plans automatically", err, t.Name)
		}
		in.AutoApprovePlan = true
	}
	return t, nil
}
