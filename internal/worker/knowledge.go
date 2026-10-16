package worker

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The knowledge layer (capability profiles §3). Every stage prompt carries
// the repo's own instructions and the skills the approved plan selected —
// ALWAYS read from the clone at a base commit (the planner's base, or the
// run's approved base), never from the worktree head: during review that
// head is the implementer's branch, and anything read from it would let the
// implementer instruct its own reviewer. The runner loads no instruction
// files itself (lockdownArgs), so this is the only source.

const (
	instructionsCap = 24 << 10 // AGENTS.md / CLAUDE.md in a prompt
	skillBodyCap    = 16 << 10 // one skill body
	skillsTotalCap  = 48 << 10 // all skill bodies in a prompt
	skillIndexMax   = 50       // skills listed to the planner
	skillDescMax    = 200      // description length in the index
	skillsDir       = ".skopos/skills"
)

// stageCommands lists the shell commands a stage may run — the Bash
// allowlist entries (base tools plus the stage profile's), as the agent
// should type them. Prompts carry it: under the lockdown the agent sees no
// settings that would tell it, and a single denied compound command once
// made an implementer conclude Bash was off entirely (end-to-end run).
func (w *Worker) stageCommands(workspace, stage string, base []string) []string {
	tools := append([]string{}, base...)
	if p := w.cfg.StageProfile(workspace, stage); p != nil {
		tools = append(tools, p.Tools...)
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range tools {
		name, pattern, ok := strings.Cut(t, "(")
		if name != "Bash" {
			continue
		}
		cmd := "any shell command"
		if ok {
			cmd = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(pattern, ")"), ":*"), "*")
		}
		if !seen[cmd] {
			seen[cmd] = true
			out = append(out, cmd)
		}
	}
	return out
}

// Skill is one repo playbook: .skopos/skills/<name>/SKILL.md.
type Skill struct {
	Name        string
	Description string
	Body        string
}

var reSkillName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// showAt reads a file at a commit from the clone ("" when absent).
func showAt(ctx context.Context, clone, base, path string) string {
	if base == "" {
		return ""
	}
	out, err := git(ctx, clone, "show", base+":"+path)
	if err != nil {
		return ""
	}
	return out
}

// repoInstructions is the repo's AGENTS.md at base, falling back to
// CLAUDE.md, capped.
func repoInstructions(ctx context.Context, clone, base string) string {
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if s := strings.TrimSpace(showAt(ctx, clone, base, f)); s != "" {
			return capText(s, instructionsCap, f)
		}
	}
	return ""
}

// repoSkills lists the skills at base, sorted by name, capped at the index
// size. The directory name is the skill's name (the frontmatter name is
// informational); malformed names are skipped.
func repoSkills(ctx context.Context, clone, base string) []Skill {
	if base == "" {
		return nil
	}
	out, err := git(ctx, clone, "ls-tree", "-d", "--name-only", base, skillsDir+"/")
	if err != nil || out == "" {
		return nil
	}
	var skills []Skill
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimPrefix(strings.TrimSpace(line), skillsDir+"/")
		if !reSkillName.MatchString(name) {
			continue
		}
		raw := showAt(ctx, clone, base, skillsDir+"/"+name+"/SKILL.md")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		desc, body := parseSkill(raw)
		skills = append(skills, Skill{Name: name, Description: desc, Body: body})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	if len(skills) > skillIndexMax {
		skills = skills[:skillIndexMax]
	}
	return skills
}

// parseSkill splits optional YAML-ish frontmatter (only `description:` is
// read) from the playbook body.
func parseSkill(raw string) (description, body string) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "---") {
		return "", s
	}
	rest := strings.TrimPrefix(s, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", s
	}
	for _, l := range strings.Split(rest[:end], "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "description" {
			description = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return description, strings.TrimSpace(rest[end+len("\n---"):])
}

// skillIndex is the planner's view: names and short descriptions.
func skillIndex(skills []Skill) []Skill {
	out := make([]Skill, len(skills))
	for i, s := range skills {
		d := s.Description
		if len(d) > skillDescMax {
			d = d[:skillDescMax] + "…"
		}
		out[i] = Skill{Name: s.Name, Description: d}
	}
	return out
}

// tagHints are the item tags that name an available skill — the
// deterministic, Martin-chosen part of the selection.
func tagHints(available []Skill, tags []string) []string {
	have := map[string]bool{}
	for _, s := range available {
		have[s.Name] = true
	}
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(t), "#"))
		if have[t] {
			out = append(out, t)
		}
	}
	return out
}

// selectSkills is the plan's final selection: the planner's picks plus the
// tag hints, restricted to skills that exist at the base, sorted, unique.
// It is resolved ONCE, at plan time, and bound into the revision — later
// stages never read tags (they stay editable after approval).
func selectSkills(available []Skill, planned, tags []string) []string {
	have := map[string]bool{}
	for _, s := range available {
		have[s.Name] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range append(append([]string{}, planned...), tagHints(available, tags)...) {
		n = strings.TrimSpace(n)
		if have[n] && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// approvedSkills returns the bodies of the named skills at base, in name
// order, each and all capped. Names that no longer exist at base are
// reported in the body so the agent knows a playbook is missing.
func approvedSkills(ctx context.Context, clone, base string, names []string) []Skill {
	if len(names) == 0 {
		return nil
	}
	all := map[string]Skill{}
	for _, s := range repoSkills(ctx, clone, base) {
		all[s.Name] = s
	}
	var out []Skill
	total := 0
	for _, n := range names {
		s, ok := all[n]
		if !ok {
			out = append(out, Skill{Name: n, Body: fmt.Sprintf("(skill %q is not present at base %s)", n, short(base))})
			continue
		}
		body := capText(s.Body, skillBodyCap, n)
		if total+len(body) > skillsTotalCap {
			out = append(out, Skill{Name: n, Body: "(omitted: the skills budget for this prompt is used up)"})
			continue
		}
		total += len(body)
		out = append(out, Skill{Name: n, Description: s.Description, Body: body})
	}
	return out
}

func capText(s string, n int, what string) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n\n[… %s truncated at %d bytes]", what, n)
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
