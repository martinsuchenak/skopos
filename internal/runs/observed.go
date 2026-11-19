package runs

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Observed is what a run saw that later rules and analyses read: the change
// an implementation made, what a plan says about itself, what the run says
// it did (shown on the item's steps and in its report). It is recorded,
// never decided on: the data the gate rules and the watcher are designed
// from (tmp/roadmap.md, item 27). Raw facts, not their reading: which files
// are migrations or CI is for whoever reads them.
type Observed struct {
	Change *Change    `json:"change,omitempty"`
	Plan   *PlanFacts `json:"plan,omitempty"`
	// Work: what the run says it did, each with what came of it — an
	// implementation's verifications, a review's checks, QA's scenarios.
	Work []WorkEntry `json:"work,omitempty"`
	// Adopted: an adopt run's commits, the ones a person added to the
	// branch after the implementation (adopt.go).
	Adopted []AdoptedCommit `json:"adopted,omitempty"`
}

// WorkEntry is one thing a run did. Result is pass, fail or skipped when
// the agent said one of them, else its own word.
type WorkEntry struct {
	Name   string `json:"name"`
	Result string `json:"result,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Change is a branch's change against its base: each file with its git
// status and line counts.
type Change struct {
	Files []ChangedFile `json:"files"`
	// Truncated: how many more files changed than Files lists.
	Truncated int `json:"truncated,omitempty"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Commits   int `json:"commits"`
}

// ChangedFile is one file of a change. Status is git's letter (A, M, D, R,
// C, T); Added and Removed are -1 for a binary file; From is a rename's or
// copy's source.
type ChangedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	From    string `json:"from,omitempty"`
}

// PlanFacts is what a plan says about itself: its size, how many steps, and
// the impact the planner declares (each a short phrase; empty = none).
type PlanFacts struct {
	Size            string   `json:"size,omitempty"`
	Steps           int      `json:"steps"`
	Irreversible    []string `json:"irreversible,omitempty"`
	BehaviourChange []string `json:"behaviour_change,omitempty"`
	OutsideBrief    []string `json:"outside_brief,omitempty"`
}

// Bounds of what a run may record.
const (
	maxObservedFiles = 1000
	maxObservedPath  = 512
	maxObservedList  = 20
	maxObservedText  = 300
	maxWorkEntries   = 60
	maxWorkName      = 500
	maxWorkNote      = 2000
	maxWorkResult    = 24
)

// clean bounds what a worker sent; nil when nothing is left.
func (o *Observed) clean() *Observed {
	if o == nil {
		return nil
	}
	out := &Observed{}
	if c := o.Change; c != nil {
		nc := &Change{Added: max(c.Added, 0), Removed: max(c.Removed, 0), Commits: max(c.Commits, 0), Truncated: max(c.Truncated, 0)}
		for _, f := range c.Files {
			if f.Path == "" {
				continue
			}
			if len(nc.Files) == maxObservedFiles {
				nc.Truncated++
				continue
			}
			nc.Files = append(nc.Files, ChangedFile{
				Path: clipText(f.Path, maxObservedPath), Status: clipText(f.Status, 4),
				Added: max(f.Added, -1), Removed: max(f.Removed, -1), From: clipText(f.From, maxObservedPath),
			})
		}
		if nc.Files == nil {
			nc.Files = []ChangedFile{}
		}
		out.Change = nc
	}
	if p := o.Plan; p != nil {
		out.Plan = &PlanFacts{
			Size: clipText(p.Size, 8), Steps: max(p.Steps, 0),
			Irreversible: cleanList(p.Irreversible), BehaviourChange: cleanList(p.BehaviourChange), OutsideBrief: cleanList(p.OutsideBrief),
		}
	}
	for _, e := range o.Work {
		e.Name, e.Note = strings.TrimSpace(e.Name), strings.TrimSpace(e.Note)
		if e.Name == "" && e.Note == "" {
			continue
		}
		if len(out.Work) == maxWorkEntries {
			break
		}
		out.Work = append(out.Work, WorkEntry{
			Name: clipText(e.Name, maxWorkName), Note: clipText(e.Note, maxWorkNote),
			Result: clipText(strings.ToLower(strings.TrimSpace(e.Result)), maxWorkResult),
		})
	}
	for _, c := range o.Adopted {
		if c.SHA = strings.TrimSpace(c.SHA); c.SHA == "" {
			continue
		}
		if len(out.Adopted) == maxWorkEntries {
			break
		}
		out.Adopted = append(out.Adopted, AdoptedCommit{SHA: clipText(c.SHA, 64), Author: clipText(strings.TrimSpace(c.Author), maxObservedText), Subject: clipText(strings.TrimSpace(c.Subject), maxWorkName)})
	}
	if out.Change == nil && out.Plan == nil && out.Work == nil && out.Adopted == nil {
		return nil
	}
	return out
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if len(out) == maxObservedList {
			break
		}
		out = append(out, clipText(s, maxObservedText))
	}
	return out
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// observedArg is the column's value for a report: NULL keeps what is stored.
func observedArg(o *Observed) any {
	o = o.clean()
	if o == nil {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	return string(b)
}
