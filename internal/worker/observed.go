package worker

import (
	"context"
	"strconv"
	"strings"
)

// What a run saw, reported for later rules and analyses (the server's
// runs.Observed: recorded, never decided on). The shapes match it.

// observedChange is a branch's change against its base.
type observedChange struct {
	Files     []changedFile `json:"files"`
	Truncated int           `json:"truncated,omitempty"`
	Added     int           `json:"added"`
	Removed   int           `json:"removed"`
	Commits   int           `json:"commits"`
}

type changedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	From    string `json:"from,omitempty"`
}

// planImpact is what the planner declares about its plan: steps whose
// effect a revert of the commit does not undo, what users or other systems
// will see behave differently, and what the plan does that the item did not
// ask for. Each a short phrase; empty when none.
type planImpact struct {
	Irreversible    []string `json:"irreversible"`
	BehaviourChange []string `json:"behaviour_change"`
	OutsideBrief    []string `json:"outside_brief"`
}

// planObserved is the plan part of what a plan run reports.
func planObserved(pr planResult) map[string]any {
	return map[string]any{"plan": map[string]any{
		"size": pr.Size, "steps": len(pr.Steps),
		"irreversible": pr.Impact.Irreversible, "behaviour_change": pr.Impact.BehaviourChange, "outside_brief": pr.Impact.OutsideBrief,
	}}
}

// maxChangedFiles bounds the files a report lists (the server keeps 1000).
const maxChangedFiles = 1000

// changeFacts is the change from base to HEAD in worktree: each file with
// its status and line counts (nil when git cannot say).
func changeFacts(ctx context.Context, worktree, base string) *observedChange {
	status, err := git(ctx, worktree, "diff", "--name-status", "-M", "-z", base+"..HEAD")
	if err != nil {
		return nil
	}
	nums, err := git(ctx, worktree, "diff", "--numstat", "-M", "-z", base+"..HEAD")
	if err != nil {
		return nil
	}
	c := &observedChange{Files: []changedFile{}, Commits: commitCount(ctx, worktree, base)}
	counts := parseNumstatZ(nums)
	for _, f := range parseNameStatusZ(status) {
		if n, ok := counts[f.Path]; ok {
			f.Added, f.Removed = n[0], n[1]
		}
		if f.Added > 0 {
			c.Added += f.Added
		}
		if f.Removed > 0 {
			c.Removed += f.Removed
		}
		if len(c.Files) == maxChangedFiles {
			c.Truncated++
			continue
		}
		c.Files = append(c.Files, f)
	}
	return c
}

// parseNameStatusZ reads `git diff --name-status -z`: a status, then one
// path, or two for a rename or copy (R100, C75: source, then destination).
func parseNameStatusZ(out string) []changedFile {
	parts := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var files []changedFile
	for i := 0; i < len(parts); {
		st := strings.TrimSpace(parts[i])
		if st == "" {
			i++
			continue
		}
		letter := st[:1]
		if (letter == "R" || letter == "C") && i+2 < len(parts) {
			files = append(files, changedFile{Status: letter, From: parts[i+1], Path: parts[i+2]})
			i += 3
			continue
		}
		if i+1 >= len(parts) {
			break
		}
		files = append(files, changedFile{Status: letter, Path: parts[i+1]})
		i += 2
	}
	return files
}

// parseNumstatZ reads `git diff --numstat -z`: "added\tremoved\tpath", or
// for a rename "added\tremoved\t" then the source and the destination as
// their own fields; "-" counts (a binary file) are -1. Keyed by the path
// the change ends at.
func parseNumstatZ(out string) map[string][2]int {
	parts := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	counts := map[string][2]int{}
	num := func(s string) int {
		if s == "-" {
			return -1
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	for i := 0; i < len(parts); i++ {
		f := strings.SplitN(parts[i], "\t", 3)
		if len(f) != 3 {
			continue
		}
		path := f[2]
		if path == "" && i+2 < len(parts) {
			path = parts[i+2]
			i += 2
		}
		counts[path] = [2]int{num(f[0]), num(f[1])}
	}
	return counts
}
