package pipeline

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/runs"
)

var (
	reFocus  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reRunner = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ValidFocus: a focus brief name (.skopos/focus/<name>.md).
func ValidFocus(name string) bool { return reFocus.MatchString(name) }

// ParseMember parses one "[focus@]runner" entry.
func ParseMember(spec string) (runs.Member, error) {
	spec = strings.TrimSpace(spec)
	focus, runner, hasFocus := strings.Cut(spec, "@")
	if !hasFocus {
		focus, runner = "", spec
	}
	m := runs.Member{Runner: strings.TrimSpace(runner), Focus: strings.TrimSpace(focus)}
	if err := validMember(m, hasFocus); err != nil {
		return runs.Member{}, err
	}
	return m, nil
}

// ParseMembers parses a comma-separated "[focus@]runner,…" list.
func ParseMembers(value string) ([]runs.Member, error) {
	var out []runs.Member
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		m, err := ParseMember(part)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one runner is required", ErrInvalidInput)
	}
	return out, nil
}

func validMember(m runs.Member, focusGiven bool) error {
	if !reRunner.MatchString(m.Runner) {
		return fmt.Errorf("%w: runner %q: letters, digits, '.', '_' or '-'", ErrInvalidInput, m.Runner)
	}
	if (focusGiven || m.Focus != "") && !ValidFocus(m.Focus) {
		return fmt.Errorf("%w: focus %q: lowercase letters, digits, '-' or '_'", ErrInvalidInput, m.Focus)
	}
	return nil
}

// FormatMembers renders members back to "[focus@]runner,…".
func FormatMembers(ms []runs.Member) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Focus != "" {
			parts = append(parts, m.Focus+"@"+m.Runner)
		} else {
			parts = append(parts, m.Runner)
		}
	}
	return strings.Join(parts, ",")
}

// multiStage: stages that may run several agents.
func multiStage(stage string) bool {
	return stage == runs.StagePlan || stage == runs.StageReview || stage == runs.StageQA
}

func knownStage(stage string) bool {
	for _, s := range Stages {
		if s == stage {
			return true
		}
	}
	return false
}

// ValidateStage checks one stage's setting with the same rules the
// --run-stage / --stage-policy flags had.
func ValidateStage(stage string, st StageSetting) error {
	if !knownStage(stage) {
		return fmt.Errorf("%w: unknown stage %q (plan, compare, synthesize, implement, review, qa)", ErrInvalidInput, stage)
	}
	if len(st.Members) == 0 {
		return fmt.Errorf("%w: %s: at least one runner is required", ErrInvalidInput, stage)
	}
	if len(st.Members) > 1 && !multiStage(stage) {
		return fmt.Errorf("%w: %s takes one runner; only plan, review and qa run several agents", ErrInvalidInput, stage)
	}
	if len(st.Members) > 8 {
		return fmt.Errorf("%w: %s: at most 8 agents", ErrInvalidInput, stage)
	}
	seen := map[string]bool{}
	for _, m := range st.Members {
		if err := validMember(m, false); err != nil {
			return fmt.Errorf("%s: %w", stage, err)
		}
		key := m.Focus + "@" + m.Runner
		if seen[key] {
			return fmt.Errorf("%w: %s lists %s twice", ErrInvalidInput, stage, FormatMembers([]runs.Member{m}))
		}
		seen[key] = true
	}
	switch st.Policy {
	case "", runs.PolicyAnyBlocks, runs.PolicyMajority, runs.PolicyAllPass:
	default:
		return fmt.Errorf("%w: %s: policy is any-blocks, majority or all-pass, not %q", ErrInvalidInput, stage, st.Policy)
	}
	if (st.Policy != "" || st.EarlyExit) && stage != runs.StageReview && stage != runs.StageQA {
		return fmt.Errorf("%w: %s: a decision policy applies only to review and qa", ErrInvalidInput, stage)
	}
	if st.TimeoutMS < 0 || (st.TimeoutMS > 0 && !multiStage(stage)) {
		return fmt.Errorf("%w: %s: a timeout applies only to plan, review and qa (and must be positive)", ErrInvalidInput, stage)
	}
	if st.TimeoutMS > 0 && time.Duration(st.TimeoutMS)*time.Millisecond < time.Minute {
		return fmt.Errorf("%w: %s: timeout must be at least one minute", ErrInvalidInput, stage)
	}
	return nil
}

// Normalize trims and validates a whole settings document.
func Normalize(in Settings) (Settings, error) {
	out := Settings{Stages: map[string]StageSetting{}}
	switch a := strings.ToLower(strings.TrimSpace(in.AgentsDefault)); a {
	case "", "default":
	case runs.AgentsPanel, runs.AgentsSolo:
		out.AgentsDefault = a
	default:
		return out, fmt.Errorf("%w: agents_default must be panel or solo, not %q", ErrInvalidInput, in.AgentsDefault)
	}
	for stage, st := range in.Stages {
		stage = strings.ToLower(strings.TrimSpace(stage))
		members := make([]runs.Member, 0, len(st.Members))
		for _, m := range st.Members {
			members = append(members, runs.Member{Runner: strings.TrimSpace(m.Runner), Focus: strings.TrimSpace(m.Focus)})
		}
		st.Members = members
		st.Policy = strings.ToLower(strings.TrimSpace(st.Policy))
		if err := ValidateStage(stage, st); err != nil {
			return out, err
		}
		out.Stages[stage] = st
	}
	return out, nil
}

// ToPrefs turns resolved stages into the run queue's routing shape.
func ToPrefs(stages map[string]StageSetting, agentsDefault string) *runs.WorkspacePrefs {
	p := &runs.WorkspacePrefs{Stages: map[string]string{}, Groups: map[string][]runs.Member{}, Policies: map[string]runs.StagePolicy{}, AgentsDefault: agentsDefault}
	for stage, st := range stages {
		switch {
		case len(st.Members) == 1:
			p.Stages[stage] = st.Members[0].Runner
			if st.Members[0].Focus != "" {
				p.Groups[stage] = st.Members // one focused agent
			}
		case len(st.Members) > 1:
			p.Groups[stage] = st.Members
		}
		if st.Policy != "" || st.EarlyExit || st.TimeoutMS > 0 {
			p.Policies[stage] = runs.StagePolicy{Policy: st.Policy, EarlyExit: st.EarlyExit, Timeout: time.Duration(st.TimeoutMS) * time.Millisecond}
		}
	}
	return p
}

// FromPrefs is the inverse, for importing the old flags.
func FromPrefs(p *runs.WorkspacePrefs) Settings {
	out := Settings{Stages: map[string]StageSetting{}}
	if p == nil {
		return out
	}
	out.AgentsDefault = p.AgentsDefault
	for stage, runner := range p.Stages {
		out.Stages[stage] = StageSetting{Members: []runs.Member{{Runner: runner}}}
	}
	for stage, members := range p.Groups {
		out.Stages[stage] = StageSetting{Members: members}
	}
	for stage, pol := range p.Policies {
		st, ok := out.Stages[stage]
		if !ok {
			continue
		}
		st.Policy, st.EarlyExit, st.TimeoutMS = pol.Policy, pol.EarlyExit, pol.Timeout.Milliseconds()
		out.Stages[stage] = st
	}
	return out
}
