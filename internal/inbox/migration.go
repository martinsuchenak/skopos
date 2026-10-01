package inbox

import (
	"context"
	"fmt"

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

// MigrateWorkflow applies the frozen tag→status table to every item holding
// a trial tag. Idempotent: items already in their target status (or without
// trial tags) are untouched; a second run is a no-op. Dry run reports
// without writing.
func (s *Service) MigrateWorkflow(ctx context.Context, dryRun bool) (*MigrationReport, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	items, err := s.store.ListItems(ctx, "", "", "", "")
	if err != nil {
		return nil, err
	}
	report := &MigrationReport{DryRun: dryRun}
	for _, item := range items {
		tag, to, hasTarget := trialTarget(item.Tags)
		if hasTrialTag(item.Tags, "agent-waiting-quota") {
			report.WaitingQuota = append(report.WaitingQuota, MigrationSkipped{
				ItemID: item.ID, Status: item.Status, Tags: item.Tags,
				Reason: "quota pause becomes a paused run in 1b; phase tag decides the status",
			})
		}
		switch {
		case !hasTarget && item.Status == StatusDiscarded:
			// Rejected trial items keep their status; the audit history is
			// backfilled so the timeline knows they were discarded.
			if !dryRun {
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
			report.Migrated = append(report.Migrated, MigrationRow{ItemID: item.ID, From: item.Status, To: to, Tag: tag})
		}
	}
	return report, nil
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
