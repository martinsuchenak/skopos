package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
)

// One-time migration from the agent-trial's tag state machine to the
// workflow statuses (docs/design/agent-pipeline.md §1, "Moving the trial
// over"). Root-only; every moved item gets one audit entry with
// via=migration so the timeline is complete from day one.

// MigrationReport summarizes one migration run.
type MigrationReport struct {
	DryRun       bool               `json:"dry_run"`
	Migrated     []MigrationRow     `json:"migrated"`
	Skipped      []MigrationSkipped `json:"skipped"`
	Unchanged    int                `json:"unchanged"`
	WaitingQuota []MigrationSkipped `json:"waiting_quota"`
}

// MigrationRow is one applied mapping.
type MigrationRow struct {
	ItemID string `json:"item_id"`
	From   Status `json:"from"`
	To     Status `json:"to"`
	Tag    string `json:"tag"`
}

// MigrationSkipped is an item the table does not cover, with why.
type MigrationSkipped struct {
	ItemID string   `json:"item_id"`
	Status Status   `json:"status"`
	Tags   []string `json:"tags"`
	Reason string   `json:"reason"`
}

// trialTagTargets maps the trial's tags to workflow statuses, most specific
// first (an item may hold agent-waiting-quota alongside its phase tag —
// that tag is noted, never a status).
var trialTagTargets = []struct {
	tag  string
	to   Status
	note string
}{
	{"done", StatusDone, ""},
	{"branch-ready", StatusInReview, ""},
	{"agent-blocked", StatusBlocked, ""},
	{"changes-requested", StatusImplementing, "change notes carried in tags/content"},
	{"agent-implementing", StatusImplementing, "resumes with resume_kind: session"},
	{"approved", StatusApproved, "approval recorded as via: migration when a revision exists"},
	{"plan-review", StatusAwaitingApproval, ""},
	{"revise", StatusPlanning, "review notes carried over"},
	{"agent-planning", StatusPlanning, "the run restarts"},
	{"ready", StatusQueued, ""},
}

// MigrateLink supplies trial-local state the server cannot know: which plan
// an in-flight item belongs to, and the base the planner read (review fix 7).
type MigrateLink struct {
	ItemID  string `json:"item_id"`
	PlanID  string `json:"plan_id"`
	BaseSHA string `json:"base_sha"`
}

// MigrateInput scopes the one-time migration (review fix 7): workspace is
// REQUIRED — the trial was single-workspace, and generic tags (done, ready)
// must not collide with unrelated items elsewhere. Links (optional) attach
// plans and snapshot revisions for plan-review items; for approved items the
// link's revision is locked and a via:migration approval is recorded.
type MigrateInput struct {
	WorkspaceID string        `json:"workspace_id"`
	DryRun      bool          `json:"dry_run"`
	Links       []MigrateLink `json:"links"`
}

// MigrateWorkflow applies the frozen tag→status table to the trial items of
// one workspace. Trial tags are STRIPPED from migrated items, which makes a
// second run a no-op (including the discarded backfill). Dry run reports
// without writing.
func (s *Service) MigrateWorkflow(ctx context.Context, input MigrateInput) (*MigrationReport, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	input.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	if input.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: workspace_id is required — the migration is scoped to the trial's workspace (generic tags must not leak across workspaces)", ErrInvalidInput)
	}
	if err := auth.RequireWorkspace(ctx, input.WorkspaceID); err != nil {
		return nil, err
	}
	links := map[string]MigrateLink{}
	for _, l := range input.Links {
		links[strings.TrimSpace(l.ItemID)] = l
	}
	items, err := s.store.ListItems(ctx, input.WorkspaceID, "", "", "")
	if err != nil {
		return nil, err
	}
	report := &MigrationReport{DryRun: input.DryRun}
	for _, item := range items {
		tag, to, hasTarget := trialTarget(item.Tags)
		if hasTrialTag(item.Tags, "agent-waiting-quota") {
			report.WaitingQuota = append(report.WaitingQuota, MigrationSkipped{
				ItemID: item.ID, Status: item.Status, Tags: item.Tags,
				Reason: "quota pause becomes a paused run in 1b; phase tag decides the status",
			})
		}
		dryRun := input.DryRun
		var kept []string
		for _, t := range item.Tags {
			if t == "agent-waiting-quota" {
				kept = append(kept, t) // informational; harmless to keep
			}
		}
		switch {
		case item.Status == StatusDiscarded:
			// Rejected trial items keep their status (whatever tags remain);
			// the audit history is backfilled and the trial tags stripped
			// (idempotency).
			if !dryRun {
				if err := s.stripTags(ctx, item, kept); err != nil {
					report.Skipped = append(report.Skipped, MigrationSkipped{ItemID: item.ID, Status: item.Status, Tags: item.Tags, Reason: err.Error()})
					continue
				}
				s.auditItem(ctx, item.ID, "inbox.migrate", audit.ViaMigration, "discarded item keeps status; audit history backfilled", "")
			}
			report.Migrated = append(report.Migrated, MigrationRow{ItemID: item.ID, From: item.Status, To: item.Status, Tag: "discarded"})
		case !hasTarget:
			report.Unchanged++
			continue
		case to == item.Status:
			report.Unchanged++
			continue
		default:
			if dryRun {
				report.Migrated = append(report.Migrated, MigrationRow{ItemID: item.ID, From: item.Status, To: to, Tag: tag})
				continue
			}
			if err := s.store.SetStatus(ctx, item.ID, to, s.now().UTC()); err != nil {
				report.Skipped = append(report.Skipped, MigrationSkipped{ItemID: item.ID, Status: item.Status, Tags: item.Tags, Reason: err.Error()})
				continue
			}
			notes := tag + " → " + string(to)
			s.auditItem(ctx, item.ID, "inbox.migrate", audit.ViaMigration, notes, "")
			s.publishItem(ctx, item.ID)
			// The link (when supplied) attaches the plan and snapshots the
			// revision the design's table requires; approved items get the
			// via:migration approval against it.
			if link, ok := links[item.ID]; ok && link.PlanID != "" && s.plans != nil {
				if _, err := s.LinkPlan(ctx, item.ID, link.PlanID); err == nil {
					if rev, rerr := s.plans.CreateRevision(ctx, link.PlanID, link.BaseSHA); rerr == nil && to == StatusApproved {
						_ = s.plans.LockRevision(ctx, rev.ID)
						if s.approvals != nil {
							_, _ = s.approvals.Record(ctx, approvals.RecordInput{
								WorkspaceID: item.WorkspaceID, ItemID: item.ID,
								Gate: approvals.GatePlan, Subject: rev.ID + "+" + rev.BaseSHA,
								Decision: approvals.DecisionApproved, Notes: "via: migration",
							})
						}
					}
				}
			}
			if err := s.stripTags(ctx, item, kept); err != nil {
				report.Skipped = append(report.Skipped, MigrationSkipped{ItemID: item.ID, Status: to, Tags: item.Tags, Reason: "status migrated but tag strip failed: " + err.Error()})
				continue
			}
			report.Migrated = append(report.Migrated, MigrationRow{ItemID: item.ID, From: item.Status, To: to, Tag: tag})
		}
	}
	return report, nil
}

// stripTags removes the trial machine tags (keeping the informational
// quota tag) so a rerun cannot match the item again.
func (s *Service) stripTags(ctx context.Context, item Item, keep []string) error {
	return s.store.UpdateItem(ctx, item.ID, item.WorkspaceID, item.Title, item.Content, tagsJSON(keep), item.Priority, s.now().UTC())
}

func trialTarget(tags []string) (tag string, to Status, ok bool) {
	for _, target := range trialTagTargets {
		for _, t := range tags {
			if t == target.tag {
				return target.tag, target.to, true
			}
		}
	}
	return "", "", false
}

func hasTrialTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// MigrateWorkflowSummary renders the one-line CLI summary.
func (r *MigrationReport) Summary() string {
	if r.DryRun {
		return fmt.Sprintf("dry run: %d would migrate, %d unchanged, %d skipped", len(r.Migrated), r.Unchanged, len(r.Skipped))
	}
	return fmt.Sprintf("migrated %d items, %d unchanged, %d skipped, %d quota-paused", len(r.Migrated), r.Unchanged, len(r.Skipped), len(r.WaitingQuota))
}
