package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/martinsuchenak/skopos/internal/policy"
)

// ErrPolicyRefuses: a save names a runner the server defines, and the
// workspace's policy allows that runner nowhere.
var ErrPolicyRefuses = errors.New("the runner policy does not allow this runner")

// SetRunnerDefs wires the runners the server defines (internal/wsconfig).
// A save that newly names one whose declared provider and model no rule of
// the workspace's policy allows is refused: the answer does not depend on
// any worker. A runner the server does not define, or one without a
// declared provider, is only warned about (the view's policy_refused).
func (s *Service) SetRunnerDefs(fn func(ctx context.Context) (map[string]policy.Facts, error)) {
	s.runnerDefs = fn
}

type runnerUse struct{ stage, runner string }

// newUses lists the stage runners (and the second-opinion default) next
// names that before did not: an old setting never blocks an unrelated edit.
func newUses(before, next Settings) []runnerUse {
	var out []runnerUse
	for stage, st := range next.Stages {
		had := map[string]bool{}
		for _, m := range before.Stages[stage].Members {
			had[m.Runner] = true
		}
		for _, m := range st.Members {
			if !had[m.Runner] && !slices.Contains(out, runnerUse{stage, m.Runner}) {
				out = append(out, runnerUse{stage, m.Runner})
			}
		}
	}
	if next.SecondOpinionRunner != "" && next.SecondOpinionRunner != before.SecondOpinionRunner {
		out = append(out, runnerUse{"second_opinion", next.SecondOpinionRunner})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].stage != out[j].stage {
			return out[i].stage < out[j].stage
		}
		return out[i].runner < out[j].runner
	})
	return out
}

func (s *Service) checkDefinedRunners(ctx context.Context, kind, id string, before, next Settings) error {
	if s.runnerDefs == nil {
		return nil
	}
	uses := newUses(before, next)
	if len(uses) == 0 {
		return nil
	}
	defs, err := s.runnerDefs(ctx)
	if err != nil {
		// The claim enforces the policy whatever happens here.
		slog.Warn("pipeline: runner definitions could not be read; the save is not checked against them", "err", err)
		return nil
	}
	if len(defs) == 0 {
		return nil
	}
	workspaces := []string{id}
	if kind == KindGroup {
		if workspaces, err = s.storage.GroupWorkspaces(ctx, id); err != nil {
			return err
		}
	}
	for _, ws := range workspaces {
		var own Settings
		if kind == KindGroup {
			// A workspace that sets the stage itself does not take the
			// group's.
			if own, err = s.storage.Get(ctx, KindWorkspace, ws); err != nil {
				return err
			}
		}
		eff, err := s.EffectivePolicy(ctx, ws)
		if err != nil {
			return err
		}
		if eff.AllowAll() {
			continue
		}
		for _, u := range uses {
			f, ok := defs[u.runner]
			if !ok || f.Provider == "" {
				continue
			}
			if kind == KindGroup {
				if _, set := own.Stages[u.stage]; set || (u.stage == "second_opinion" && own.SecondOpinionRunner != "") {
					continue
				}
			}
			if d := eff.CouldAllow(f, u.stage); !d.Allowed {
				return fmt.Errorf("%w: %s (%s) in the %s stage of %s: the %s policy allows it on no worker", ErrPolicyRefuses, u.runner, describeFacts(f), u.stage, ws, d.Refused)
			}
		}
	}
	return nil
}
