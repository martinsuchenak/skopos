package apikeys

import (
	"context"
	"sort"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
)

// Key and workspace-group changes are audit events: who created, changed,
// revoked or deleted a key, and what it may do afterwards — above all a
// grant of the approver or admin permission — and who changed a group,
// which is part of the scope of every key holding it. The note never
// carries a secret (a key's exists only in the create answer).

// AuditRecorder is the audit service's write side.
type AuditRecorder interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// SetAuditRecorder makes key changes audit events (nil = not recorded).
func (s *Service) SetAuditRecorder(r AuditRecorder) { s.audit = r }

type viaKey struct{}

// WithVia labels the surface a key change arrived through (the handler
// passes the request's X-Skopos-Via).
func WithVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

func viaOf(ctx context.Context) string {
	v, _ := ctx.Value(viaKey{}).(string)
	return v
}

// record writes one key event; a failed write never fails the change
// itself (the audit package's rule for every caller).
func (s *Service) record(ctx context.Context, action string, key Key, notes string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Record(ctx, audit.RecordInput{
		EntityType: "api_key", EntityID: key.ID, Action: action, Via: viaOf(ctx), Notes: notes,
	})
}

// recordGroup writes one workspace-group event. A group's members and
// patterns are part of the scope of every key holding it, so a change to
// them changes what those keys reach.
func (s *Service) recordGroup(ctx context.Context, action, groupID, notes string) {
	if s.audit == nil || groupID == "" {
		return
	}
	_ = s.audit.Record(ctx, audit.RecordInput{
		EntityType: "workspace_group", EntityID: groupID, Action: action, Via: viaOf(ctx), Notes: notes,
	})
}

// describeGroup is a group's name, members and patterns as one line.
func describeGroup(g Group) string {
	members := append([]string{}, g.Members...)
	sort.Strings(members)
	patterns := append([]string{}, g.Patterns...)
	sort.Strings(patterns)
	return "name=" + g.Name + " members=" + strings.Join(members, ",") + " patterns=" + strings.Join(patterns, ",")
}

// describeKey is what a key may do, as one line for the audit note.
func describeKey(k Key) string {
	parts := []string{"name=" + k.Name}
	switch {
	case k.AllWorkspaces:
		parts = append(parts, "scope=*")
	default:
		ws := append([]string{}, k.Workspaces...)
		sort.Strings(ws)
		parts = append(parts, "workspaces="+strings.Join(ws, ","))
		if len(k.Groups) > 0 {
			gs := append([]string{}, k.Groups...)
			sort.Strings(gs)
			parts = append(parts, "groups="+strings.Join(gs, ","))
		}
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{k.Approver, "approver"}, {k.Admin, "admin"}, {k.Delegate, "delegate"}} {
		if f.on {
			parts = append(parts, f.name)
		}
	}
	if k.Capabilities == nil {
		parts = append(parts, "capabilities=full")
	} else {
		parts = append(parts, "capabilities="+strings.Join(k.Capabilities, ","))
	}
	return strings.Join(parts, " ")
}
