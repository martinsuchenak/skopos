package worker

import (
	"strings"
	"testing"
)

// Both advisory prompts render with the round's candidates (review steps
// left out), and the comparison renders scores by candidate name.
func TestAdvisoryPromptsAndComparison(t *testing.T) {
	cands := []Revision{
		{Label: "claude", Steps: []RevStep{{Title: "patch", Phase: "implement"}, {Title: "Review and push (Martin)", Phase: "review"}}},
		{Label: "glm", Steps: []RevStep{{Title: "rewrite", Description: "module x", Phase: "implement"}}, Skills: []string{"go-tests"}},
	}
	views := candidateViews(cands)
	for _, name := range []string{"compare.md", "synthesize.md"} {
		out, err := renderPrompt(name, map[string]any{"Title": "t", "Content": "c", "Candidates": views, "Comparison": "**Recommendation:** 2", "Instructions": "Use mago."})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, want := range []string{"## Candidate 1 · claude", "1. patch", "## Candidate 2 · glm", "1. rewrite — module x", "Use mago."} {
			if !strings.Contains(out, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
		if strings.Contains(out, "Review and push") {
			t.Errorf("%s must not list the server-owned review step", name)
		}
	}
	var cr compareResult
	cr.Summary, cr.Recommendation, cr.Reasons = "glm covers tests", "2 · glm", []string{"adds tests"}
	cr.Scores = append(cr.Scores, struct {
		Candidate   int      `json:"candidate"`
		Correctness int      `json:"correctness"`
		Scope       int      `json:"scope"`
		Granularity int      `json:"granularity"`
		Tests       int      `json:"tests"`
		Fit         int      `json:"fit"`
		Risks       []string `json:"risks"`
	}{Candidate: 2, Correctness: 4, Scope: 5, Granularity: 4, Tests: 5, Fit: 4, Risks: []string{"touches auth"}})
	sec := comparisonSection(cr, views)
	for _, want := range []string{"**Recommendation:** 2 · glm", "- adds tests", "| 2 · glm | 4 | 5 | 4 | 5 | 4 |", "Risk (candidate 2): touches auth"} {
		if !strings.Contains(sec, want) {
			t.Errorf("comparison missing %q:\n%s", want, sec)
		}
	}
}
