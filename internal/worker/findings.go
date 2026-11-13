package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/martinsuchenak/skopos/internal/findings"
)

// Findings are records on the server (internal/findings): a run's prompt
// lists the earlier ones it has to act on, and its result answers each by
// id. In a prompt a finding goes by a short handle (F1, F2, …) instead of
// its id — an agent copies "F3" back reliably, a UUID it does not.

// findingRef is a finding in a prompt.
type findingRef struct {
	Handle string
	findings.Entry
}

// ClaimText is the implementer's word on the finding, for a reviewer.
func (r findingRef) ClaimText() string {
	switch r.Claim {
	case findings.KindFixed:
		return "fixed"
	case findings.KindNotFixed:
		return "not fixed"
	}
	return ""
}

// findingAnswer is an agent's answer to a finding in its prompt.
type findingAnswer struct {
	ID     string
	Status string
	Note   string
}

// findingAnswers is the list of them as an agent wrote it. Reading it never
// fails: answers are an extra on a run's result, and a list in a shape of
// its own (no list at all, a number for an id, an entry that is no object)
// must not cost the run its result. What cannot be read is left out;
// resolveAnswers then keeps only what names a finding the prompt listed.
type findingAnswers []findingAnswer

func (a *findingAnswers) UnmarshalJSON(raw []byte) error {
	*a = nil
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	for _, item := range list {
		var m map[string]any
		if json.Unmarshal(item, &m) != nil {
			continue
		}
		*a = append(*a, findingAnswer{ID: scalar(m["id"]), Status: scalar(m["status"]), Note: scalar(m["note"])})
	}
	return nil
}

// scalar is a JSON value an agent wrote where a string was asked for.
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func findingRefs(es []findings.Entry) []findingRef {
	out := make([]findingRef, 0, len(es))
	for i, e := range es {
		out = append(out, findingRef{Handle: fmt.Sprintf("F%d", i+1), Entry: e})
	}
	return out
}

// answered is one finding with what the run said about it.
type answered struct {
	Ref    findingRef
	Status string
	Note   string
}

// resolveAnswers matches an agent's answers to the findings its prompt
// listed, by handle. An answer for a handle the prompt did not have, with a
// status the stage does not give, or a second one for the same finding, is
// left out.
func resolveAnswers(refs []findingRef, in []findingAnswer, statuses ...string) []answered {
	byHandle := map[string]findingRef{}
	for _, r := range refs {
		byHandle[r.Handle] = r
	}
	var out []answered
	seen := map[string]bool{}
	for _, a := range in {
		handle := strings.ToUpper(strings.Trim(strings.TrimSpace(a.ID), "`'\"#()[] "))
		if _, err := strconv.Atoi(handle); err == nil {
			handle = "F" + handle // "2" for F2
		}
		ref, ok := byHandle[handle]
		status := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(a.Status), " ", "_"))
		allowed := false
		for _, s := range statuses {
			allowed = allowed || s == status
		}
		if !ok || !allowed || seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, answered{Ref: ref, Status: status, Note: strings.TrimSpace(a.Note)})
	}
	return out
}

// wireAnswers is the answers as the run's report sends them.
func wireAnswers(as []answered) []findings.Answer {
	out := make([]findings.Answer, 0, len(as))
	for _, a := range as {
		out = append(out, findings.Answer{ID: a.Ref.ID, Status: a.Status, Note: a.Note})
	}
	return out
}

// stillOpenBlocking counts the earlier blocking findings a review found
// not fixed: they fail it like its own blocking findings.
func stillOpenBlocking(as []answered) int {
	n := 0
	for _, a := range as {
		if a.Status == findings.KindStillOpen && a.Ref.Severity == findings.SeverityBlocking {
			n++
		}
	}
	return n
}

// runFindings reads the findings the run acts on. nil: the server holds no
// record (it predates it) or could not be read — the run then works as it
// did before findings were records, and its log says so.
func (w *Worker) runFindings(ctx context.Context, run *Run) *findings.View {
	view, ok, err := w.client.ReadFindings(ctx, run.ID, w.cfg.ID)
	if err != nil {
		runLogFrom(ctx).notef("the record of earlier findings could not be read (%v): this run does not answer them", err)
		return nil
	}
	if !ok {
		return nil
	}
	return view
}
