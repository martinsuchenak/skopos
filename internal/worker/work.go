package worker

import (
	"encoding/json"
	"fmt"
	"strings"
)

// workEntry is one thing a run did and what came of it: a QA scenario, a
// check a review made, a verification the implementer ran. The run reports
// its list as runs.Observed.Work (the same shape on the wire).
type workEntry struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Note   string `json:"note"`
}

// workList is an agent's list of what it did. It is read by a decoder that
// takes any shape and never errors: a malformed list must never cost a run
// its result (the finding answers' rule). Results are normalised to pass,
// fail or skipped where the word allows.
type workList []workEntry

func (l *workList) UnmarshalJSON(raw []byte) error {
	*l = nil
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	for _, item := range list {
		var m map[string]any
		if json.Unmarshal(item, &m) != nil {
			continue
		}
		e := workEntry{Name: strings.TrimSpace(scalar(m["name"])), Result: workResult(scalar(m["result"])), Note: strings.TrimSpace(scalar(m["note"]))}
		if e.Name == "" && e.Note == "" {
			continue
		}
		*l = append(*l, e)
	}
	return nil
}

// workResult is an agent's word for an outcome, as pass, fail or skipped
// when it says one of them; anything else is kept as written.
func workResult(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "pass", "passed", "ok", "ran", "matches", "holds":
		return "pass"
	case "fail", "failed", "differs":
		return "fail"
	case "skip", "skipped", "not run", "not_run", "not checked", "could not run":
		return "skipped"
	}
	return s
}

// redacted is the list with the worker's known secrets taken out of every
// entry (red nil = nothing to take out).
func (l workList) redacted(red *strings.Replacer) workList {
	if red == nil || len(l) == 0 {
		return l
	}
	out := make(workList, len(l))
	for i, e := range l {
		out[i] = workEntry{Name: red.Replace(e.Name), Result: e.Result, Note: red.Replace(e.Note)}
	}
	return out
}

// wire is the list as the report sends it (nil when empty).
func (l workList) wire() []workEntry {
	if len(l) == 0 {
		return nil
	}
	return l
}

// workSection lists what a run did, for its section in the item.
func workSection(title string, l workList) string {
	var b strings.Builder
	for _, e := range l {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		if b.Len() == 0 {
			fmt.Fprintf(&b, "\n\n**%s**\n", title)
		}
		result := e.Result
		if result == "" {
			result = "not reported"
		}
		fmt.Fprintf(&b, "\n- %s: **%s**", name, result)
		if note := strings.TrimSpace(e.Note); note != "" {
			fmt.Fprintf(&b, " — %s", note)
		}
	}
	return b.String()
}

// addWork adds a run's list to the observed facts its report carries.
func addWork(fields map[string]any, l workList) map[string]any {
	if len(l) == 0 {
		return fields
	}
	observed, _ := fields["observed"].(map[string]any)
	if observed == nil {
		observed = map[string]any{}
	}
	observed["work"] = l.wire()
	fields["observed"] = observed
	return fields
}
