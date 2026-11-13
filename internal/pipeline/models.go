// Package pipeline stores which agents run each workflow stage, per
// workspace or per workspace group, and resolves the effective routing the
// run queue reads at every queue (tmp/workspace-pipeline-settings —
// design.md). Resolution per stage: the workspace's own setting, else its
// group's, else not set.
package pipeline

import (
	"errors"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// Owner kinds: settings belong to a workspace or to a workspace group.
const (
	KindWorkspace = "workspace"
	KindGroup     = "group"
)

// Stages lists the configurable stages in workflow order.
var Stages = []string{runs.StagePlan, runs.StageSynthesize, runs.StageCompare, runs.StageImplement, runs.StageReview, runs.StageQA}

var (
	ErrInvalidInput = errors.New("invalid pipeline settings")
	ErrNotFound     = errors.New("not found")
	// ErrConflict: a workspace would get the same stage from two groups.
	ErrConflict = errors.New("pipeline conflict")
)

// StageSetting is one stage's routing: its members (a list only on plan,
// review and qa) and, for a group stage, how it is decided.
type StageSetting struct {
	Members   []runs.Member `json:"members"`
	Policy    string        `json:"policy,omitempty"`
	EarlyExit bool          `json:"early_exit,omitempty"`
	TimeoutMS int64         `json:"timeout_ms,omitempty"`
}

// Settings is everything one owner stores. A stage missing from Stages is
// not set at this level (a workspace then inherits its group's).
type Settings struct {
	Stages        map[string]StageSetting `json:"stages"`
	AgentsDefault string                  `json:"agents_default,omitempty"`
	// SecondOpinionRunner is the runner a plain "second opinion" request
	// uses (dashboard preselect, Slack `second`); "" = none at this level.
	SecondOpinionRunner string `json:"second_opinion_runner,omitempty"`
	// CodeIndex: idle workers keep this workspace's code index fresh
	// (tmp/agent-mcp-and-indexing — design.md §3). Workspace owners only;
	// nil = off.
	CodeIndex *bool `json:"code_index,omitempty"`
}

// EffectiveStage is a resolved stage with where it came from:
// "workspace" or "group:<name>".
type EffectiveStage struct {
	StageSetting
	Source string `json:"source"`
}

// GroupRef names a group that contains a workspace.
type GroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// View is the read model: the owner's own settings plus, for a workspace,
// the effective routing with sources, the groups it inherits from, and any
// stage two groups both set (resolved by group name until fixed).
type View struct {
	OwnerKind              string                    `json:"owner_kind"`
	OwnerID                string                    `json:"owner_id"`
	Own                    Settings                  `json:"own"`
	Effective              map[string]EffectiveStage `json:"effective,omitempty"`
	EffectiveAgentsDefault string                    `json:"effective_agents_default,omitempty"`
	AgentsDefaultSource    string                    `json:"agents_default_source,omitempty"`
	// Effective default second-opinion runner, and where it comes from.
	EffectiveSecondOpinionRunner string     `json:"effective_second_opinion_runner,omitempty"`
	SecondOpinionSource          string     `json:"second_opinion_source,omitempty"`
	Groups                       []GroupRef `json:"groups,omitempty"`
	Conflicts                    []string   `json:"conflicts,omitempty"`
	// UnknownRunners: effective runners no worker serving this workspace
	// offers yet — their runs wait until one does.
	UnknownRunners []string `json:"unknown_runners,omitempty"`
	// Policy: every runner policy that applies to the workspace, with
	// where each comes from (policy.go); PolicyRefused: configured runners
	// the policy does not allow on any worker that offers them — their
	// runs would wait for a worker that may not take them.
	Policy        *EffectivePolicyView `json:"policy,omitempty"`
	PolicyRefused []string             `json:"policy_refused,omitempty"`
	// IndexableBy: the workers that can fetch this workspace's repo now
	// (their last claim said so) — the code_index switch needs one.
	IndexableBy []string `json:"indexable_by"`
}
