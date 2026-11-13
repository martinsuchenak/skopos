package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/findings"
)

// `skopos inbox findings` prints a count by state, then each finding with
// its id (what --finding takes) and what happened to it.
func TestPrintFindings(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local)
	fs := []findings.Finding{
		{ID: "id-open", Stage: "review", Focus: "security", HeadSHA: "aaa1111222", Severity: "blocking", Text: "counts per process", File: "limit.go:41", State: findings.StateStillOpen,
			Updates: []findings.Update{
				{Kind: findings.KindSentBack, Actor: "alex (key ops)", CreatedAt: at},
				{Kind: findings.KindFixed, Note: "moved the counter", CreatedAt: at},
				{Kind: findings.KindStillOpen, Note: "two statements", CreatedAt: at},
				{Kind: findings.KindNotFixed, Note: "needs a schema change", CreatedAt: at},
			}},
		{ID: "id-done", Stage: "second_opinion", Runner: "codex", Severity: "blocking", Text: "race", State: findings.StateAddressed,
			Updates: []findings.Update{{Kind: findings.KindAddressed, CreatedAt: at}}},
		{ID: "id-waived", Stage: "qa", Severity: "nit", Text: "spacing", State: findings.StateWaived,
			Updates: []findings.Update{{Kind: findings.KindWaived, Actor: "alex", Note: "fine", CreatedAt: at}}},
	}
	var b bytes.Buffer
	printFindings(&b, fs, "")
	out := b.String()
	for _, want := range []string{
		"3 finding(s): 1 still open, 1 addressed, 1 waived",
		"id-open  [still open] blocking, review · security @ aaa1111  limit.go:41",
		"  counts per process",
		"sent back to be fixed by alex (key ops)",
		"the implementer says fixed: moved the counter",
		"a review found it not fixed: two statements",
		"the implementer left it: needs a schema change",
		"id-done  [addressed] blocking, second opinion · codex",
		"a review confirmed it is fixed",
		"waived by alex: fine",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	printFindings(&b, fs, findings.StateWaived)
	if out := b.String(); !strings.Contains(out, "id-waived") || strings.Contains(out, "id-open  [") || !strings.Contains(out, "3 finding(s)") {
		t.Fatalf("--state narrows the list, not the count:\n%s", out)
	}
	b.Reset()
	printFindings(&b, nil, "")
	if !strings.Contains(b.String(), "No review findings") {
		t.Fatalf("an item without findings: %q", b.String())
	}
	if got := oneLine("a  long\ntext that goes on", 10); got != "a long te…" {
		t.Fatalf("oneLine: %q", got)
	}
}
