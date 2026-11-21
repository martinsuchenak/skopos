package cards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemstate"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/runs"
)

func state(status inbox.Status) *itemstate.State {
	st := &itemstate.State{
		Item:      itemstate.ItemView{ID: "01item", WorkspaceID: "github.com/acme/app", Title: "Add a setting", Ref: "JOB-1", Status: string(status)},
		Questions: []itemstate.QuestionView{},
		Reviews:   []itemstate.ReviewView{},
	}
	switch status {
	case inbox.StatusAwaitingApproval:
		st.Turn, st.Gate = itemstate.TurnPerson, itemstate.GatePlanApproval
	case inbox.StatusInReview:
		st.Turn, st.Gate = itemstate.TurnPerson, itemstate.GateSignOff
	case inbox.StatusBlocked:
		st.Turn, st.Gate = itemstate.TurnPerson, itemstate.GateBlocked
	case inbox.StatusFailed:
		st.Turn, st.Gate = itemstate.TurnPerson, itemstate.GateFailed
	case inbox.StatusOpen:
		st.Turn = itemstate.TurnPerson
	case inbox.StatusDone, inbox.StatusDiscarded:
		st.Turn = itemstate.TurnNone
	default:
		st.Turn = itemstate.TurnAgent
	}
	return st
}

func revision(id, label string, titles ...string) itemstate.RevisionView {
	r := itemstate.RevisionView{RevisionID: id, Label: label, ContentHash: "h" + id}
	for i, t := range titles {
		r.Steps = append(r.Steps, plans.RevisionStep{ID: t, Title: t, Position: i, Description: "do " + t})
	}
	return r
}

// texts is every Text component's text, in order.
func texts(t *testing.T, env *Envelope) []string {
	t.Helper()
	var out []string
	for _, c := range components(t, env) {
		if c["component"] == "Text" {
			out = append(out, c["text"].(string))
		}
	}
	return out
}

func components(t *testing.T, env *Envelope) []map[string]any {
	t.Helper()
	b, err := json.Marshal(env.A2UI)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []struct {
		Version       string `json:"version"`
		DeleteSurface *struct {
			SurfaceID string `json:"surfaceId"`
		} `json:"deleteSurface"`
		CreateSurface *struct {
			SurfaceID  string           `json:"surfaceId"`
			CatalogID  string           `json:"catalogId"`
			Components []map[string]any `json:"components"`
		} `json:"createSurface"`
	}
	if err := json.Unmarshal(b, &msgs); err != nil || len(msgs) != 2 {
		t.Fatalf("deleteSurface, then createSurface: %v %s", err, b)
	}
	del, create := msgs[0], msgs[1]
	if del.Version != "1.0" || create.Version != "1.0" || del.DeleteSurface == nil || create.CreateSurface == nil ||
		create.CreateSurface.CatalogID != BasicCatalog || create.CreateSurface.SurfaceID == "" ||
		del.DeleteSurface.SurfaceID != create.CreateSurface.SurfaceID {
		t.Fatalf("message heads: %s", b)
	}
	return create.CreateSurface.Components
}

// conforms checks a surface against the basic catalog v1 itself
// (testdata/basic_catalog_v1.json, the published catalog): every component
// is in the catalog, has its required properties and no others, every
// child reference names a component, ids are unique, and root exists.
func conforms(t *testing.T, env *Envelope) {
	t.Helper()
	raw, err := os.ReadFile("testdata/basic_catalog_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	type part struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	var cat struct {
		Components map[string]struct {
			part
			AllOf []part `json:"allOf"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	comps := components(t, env)
	ids := map[string]bool{}
	for _, c := range comps {
		id, _ := c["id"].(string)
		if id == "" || ids[id] {
			t.Fatalf("component id missing or repeated: %v", c)
		}
		ids[id] = true
	}
	if !ids["root"] {
		t.Fatal("no root component")
	}
	referenced := map[string]bool{"root": true}
	ref := func(id any) {
		s, ok := id.(string)
		if !ok || !ids[s] {
			t.Fatalf("a child reference names no component: %v", id)
		}
		referenced[s] = true
	}
	for _, c := range comps {
		kind, _ := c["component"].(string)
		def, ok := cat.Components[kind]
		if !ok {
			t.Fatalf("component %q is not in the basic catalog", kind)
		}
		allowed := map[string][]string{"id": nil, "component": nil, "weight": nil}
		var required []string
		for _, pt := range append([]part{def.part}, def.AllOf...) {
			for k, prop := range pt.Properties {
				allowed[k] = prop.Enum
			}
			required = append(required, pt.Required...)
		}
		for k, v := range c {
			enum, ok := allowed[k]
			if !ok {
				t.Fatalf("%s has a property the catalog does not define: %s", kind, k)
			}
			if s, isString := v.(string); isString && len(enum) > 0 && !slices.Contains(enum, s) {
				t.Fatalf("%s.%s = %q is not one of %v", kind, k, s, enum)
			}
		}
		for _, k := range required {
			if _, ok := c[k]; !ok {
				t.Fatalf("%s lacks the required %s: %v", kind, k, c)
			}
		}
		switch kind {
		case "Column", "Row", "List":
			for _, ch := range c["children"].([]any) {
				ref(ch)
			}
		case "Card", "Button":
			ref(c["child"])
		case "Tabs":
			for _, tb := range c["tabs"].([]any) {
				m := tb.(map[string]any)
				ref(m["child"])
				if s, _ := m["title"].(string); s == "" {
					t.Fatalf("a tab without a title: %v", m)
				}
			}
		case "Text":
			if s, _ := c["text"].(string); s == "" {
				t.Fatalf("an empty Text: %v", c)
			}
		}
	}
	for id := range ids {
		if !referenced[id] {
			t.Fatalf("component %s is in the surface but nothing shows it", id)
		}
	}
}

func TestCardPerStatus(t *testing.T) {
	gate := state(inbox.StatusAwaitingApproval)
	gate.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{revision("r1", "claude", "Add the action", "Test it")}}
	gate.Questions = []itemstate.QuestionView{
		{ID: "q1", SetID: "s1", Asker: "planner", Label: "claude", Text: "Drafts too?", Options: []string{"only on submit", "on every save"}, Blocking: true},
		{ID: "q2", SetID: "s1", Asker: "planner", Label: "claude", Text: "Preload setting here?"},
	}
	panel := state(inbox.StatusAwaitingApproval)
	panel.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{revision("r1", "claude", "A"), revision("r2", "codex", "B")}}

	blocked := state(inbox.StatusBlocked)
	blocked.Reason = "qa needs an answer: no test form\nmore detail"
	blocked.Questions = []itemstate.QuestionView{{ID: "q7", SetID: "s7", Asker: "worker", Text: "Which form?", Options: []string{"Site Data", "a new one"}}}

	signOff := state(inbox.StatusInReview)
	signOff.Implementation = &itemstate.Implementation{HeadSHA: "3f9a2c1d00", PRURL: "https://github.com/acme/app/pull/7",
		Checks: []runs.Check{{Name: "lint", Passed: true}, {Name: "test", Passed: true}}}
	signOff.Reviews = []itemstate.ReviewView{{RunID: "rv1", Stage: runs.StageReview, Runner: "claude", Status: "succeeded", Blocking: 1, Nits: 2}}
	signOff.Findings = &itemstate.FindingsView{Open: []itemstate.FindingView{{ID: "f1", Severity: "blocking", Text: "no size limit", File: "a.php"}}}

	brokenTitle := state(inbox.StatusAwaitingApproval)
	r := revision("r9", "", "x")
	r.Steps[0].Title, r.Steps[0].Description = "Update config\n- add key", "do Update config"
	brokenTitle.Plan = &itemstate.PlanView{Revisions: []itemstate.RevisionView{r}}

	working := state(inbox.StatusImplementing)
	working.Run = &itemstate.RunView{Stage: runs.StageImplement, Status: "running", Runner: "claude"}
	queued := state(inbox.StatusQueued)
	queued.Run = &itemstate.RunView{Stage: runs.StagePlan, Status: "queued"}

	for _, tc := range []struct {
		name      string
		in        Input
		status    string
		attention bool
		summary   string
		texts     []string // each must appear in some Text
	}{
		{"no item", Input{Subject: Subject{Ref: "JOB-9"}}, StatusNone, false, "Not sent to skopos", []string{"`JOB-9`"}},
		{"not started", Input{State: state(inbox.StatusOpen)}, StatusNotStarted, true, "Not started", nil},
		{"queued", Input{State: queued}, StatusAgent, false, "Skopos is working on it: planning", []string{"Queued: planning, waiting for a worker.", "**Plan**"}},
		{"working", Input{State: working}, StatusAgent, false, "Skopos is working on it: implementing", []string{"claude is on it.", "**Implement**"}},
		{"plan approval", Input{State: gate}, StatusPlanApproval, true, "Plan ready for approval — 2 questions",
			[]string{"1. **Add the action**\\\n   do Add the action\n2. **Test it**", "#### Questions — planner claude", "The work waits for the answers.",
				"1. Drafts too?\n   - (a) only on submit\n   - (b) on every save\n2. Preload setting here?"}},
		{"plan approval, two plans", Input{State: panel}, StatusPlanApproval, true, "Plan ready for approval — 2 plans", nil},
		{"plan approval, nothing to do", Input{State: state(inbox.StatusAwaitingApproval), NothingToDo: "already done upstream"}, StatusPlanApproval, true,
			"Waiting for you: the planner found nothing to do — already done upstream", []string{"already done upstream"}},
		{"blocked", Input{State: blocked}, StatusBlocked, true, "Blocked: qa needs an answer: no test form", []string{"more detail", "#### Questions — QA", "#### Why it stopped"}},
		{"failed", Input{State: state(inbox.StatusFailed)}, StatusFailed, true, "Failed", nil},
		{"sign-off", Input{State: signOff}, StatusSignOff, true, "Branch ready for sign-off — 1 blocking finding open",
			[]string{"pull request #7 · commit `3f9a2c1`", "- Pull request: https://github.com/acme/app/pull/7", "Checks: passed — lint, test", "Review (claude): **1 blocking**, 2 nits", "- **blocking** no size limit (`a.php`)"}},
		{"done, merged", Input{State: state(inbox.StatusDone), Outcome: &inbox.BranchOutcome{State: inbox.OutcomeMerged}}, StatusDone, false, "Done, merged", nil},
		{"done, nothing to do", Input{State: state(inbox.StatusDone), NothingToDo: "nothing changed"}, StatusNothingToDo, false, "Closed: nothing to do — nothing changed", nil},
		{"discarded", Input{State: state(inbox.StatusDiscarded)}, StatusDiscarded, false, "Discarded", nil},
		{"converted", Input{State: state(inbox.StatusConverted)}, StatusDone, false, "Done: converted into a plan", []string{"converted into a plan"}},
		{"a step title with a line break", Input{State: brokenTitle}, StatusPlanApproval, true, "Plan ready for approval", []string{"1. **Update config**\\\n   do Update config"}},
	} {
		env := Build(tc.in)
		if env.Status != tc.status || env.Attention != tc.attention || env.Summary != tc.summary || env.Key == "" {
			t.Errorf("%s: %s %v %q key %q", tc.name, env.Status, env.Attention, env.Summary, env.Key)
		}
		conforms(t, env)
		all := strings.Join(texts(t, env), "\n----\n")
		for _, want := range tc.texts {
			if !strings.Contains(all, want) {
				t.Errorf("%s: no Text holds %q:\n%s", tc.name, want, all)
			}
		}
		if !slices.Contains(Statuses, env.Status) {
			t.Errorf("%s: status %q is not listed", tc.name, env.Status)
		}
	}
	tabs := 0
	for _, c := range components(t, Build(Input{State: panel})) {
		if c["component"] == "Tabs" {
			tabs = len(c["tabs"].([]any))
		}
	}
	if tabs != 2 {
		t.Fatalf("two plans are two tabs: %d", tabs)
	}
}

// The key changes exactly when something new happened, and the same state
// builds the same envelope.
// byKind is a card's components by kind.
func byKind(t *testing.T, env *Envelope) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, c := range components(t, env) {
		out[c["component"].(string)] = append(out[c["component"].(string)], c)
	}
	return out
}

// buttons are a card's buttons: action name and variant, in order.
func buttons(t *testing.T, env *Envelope) []string {
	t.Helper()
	var out []string
	for _, c := range byKind(t, env)["Button"] {
		out = append(out, c["action"].(map[string]any)["event"].(map[string]any)["name"].(string)+"/"+c["variant"].(string))
	}
	return out
}

func dataModel(env *Envelope) map[string]any {
	dm, _ := env.A2UI[1].(map[string]any)["createSurface"].(map[string]any)["dataModel"].(map[string]any)
	return dm
}

// A key that may act gets a button per action the state allows, and the
// inputs they take: the open questions (options and a text field each,
// under the question's id), the plan to pick when there are several, the
// open findings to send back, a notes field. Without actions the card is
// text, as before.
func TestActionForm(t *testing.T) {
	gate := state(inbox.StatusAwaitingApproval)
	gate.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{revision("r1", "claude", "Add the action")}}
	gate.Questions = []itemstate.QuestionView{
		{ID: "q1", SetID: "s1", N: 1, Asker: "planner", Text: "Drafts too?", Options: []string{"only on submit", "on every save"}},
		{ID: "q2", SetID: "s1", N: 2, Asker: "planner", Text: "Anything to keep?"},
	}
	gate.Actions = []string{"approve", "request-changes", "answers", "reject"}
	env := Build(Input{State: gate, Actions: true})
	conforms(t, env)
	kinds := byKind(t, env)
	if len(kinds["ChoicePicker"]) != 1 || len(kinds["TextField"]) != 3 || len(kinds["CheckBox"]) != 0 {
		t.Fatalf("the inputs: two questions and the notes: %v", kinds)
	}
	pick := kinds["ChoicePicker"][0]
	if pick["value"].(map[string]any)["@path"] != "/answers/q1/option" ||
		pick["options"].([]any)[1].(map[string]any)["value"] != "b" || pick["options"].([]any)[1].(map[string]any)["label"] != "(b) on every save" {
		t.Fatalf("the options: %v", pick)
	}
	if kinds["TextField"][2]["value"].(map[string]any)["@path"] != "/notes" {
		t.Fatalf("the notes field: %v", kinds["TextField"][2])
	}
	if got := buttons(t, env); fmt.Sprint(got) != "[approve/primary request-changes/default answers/default reject/borderless]" {
		t.Fatalf("the buttons: %v", got)
	}
	ctx := kinds["Button"][0]["action"].(map[string]any)["event"].(map[string]any)["context"].(map[string]any)
	if ctx["item"] != "01item" || ctx["key"] != env.Key || ctx["answers"].(map[string]any)["@path"] != "/answers" ||
		ctx["notes"].(map[string]any)["@path"] != "/notes" || ctx["plan"] != nil {
		t.Fatalf("the context: the item and the card's key, the inputs, no plan to pick for one plan: %v", ctx)
	}
	dm := dataModel(env)
	if len(dm["answers"].(map[string]any)) != 2 || dm["notes"] != "" {
		t.Fatalf("the data model: %v", dm)
	}
	all := strings.Join(texts(t, env), "\n")
	if strings.Contains(all, "(a) only on submit") || !strings.Contains(all, "**Plan approval**") {
		t.Fatalf("options are inputs, not text as well; the tracker shows the step: %s", all)
	}

	// Several plans: pick the one to approve.
	panel := state(inbox.StatusAwaitingApproval)
	panel.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{revision("r1", "claude", "A"), revision("r2", "codex", "B")}}
	panel.Actions = gate.Actions
	env = Build(Input{State: panel, Actions: true})
	conforms(t, env)
	picker := byKind(t, env)["ChoicePicker"]
	if len(picker) != 1 || picker[0]["label"] != "The plan to approve" || picker[0]["options"].([]any)[1].(map[string]any)["value"] != "r2" {
		t.Fatalf("the plan picker: %v", picker)
	}

	// A stopped gate: continue first; the question keeps its number.
	blocked := state(inbox.StatusBlocked)
	blocked.Questions = []itemstate.QuestionView{{ID: "q7", SetID: "s7", N: 2, Asker: "worker", Text: "Which form?"}}
	blocked.Actions = []string{"retry", "answers", "reject"}
	env = Build(Input{State: blocked, Actions: true})
	conforms(t, env)
	if got := buttons(t, env); fmt.Sprint(got) != "[retry/primary answers/default reject/borderless]" {
		t.Fatalf("a stopped gate: %v", got)
	}
	if all := strings.Join(texts(t, env), "\n"); !strings.Contains(all, "**2.** Which form?") || !strings.Contains(all, "Send answers and continue") {
		t.Fatalf("a stopped gate's text: %s", all)
	}

	// Sign-off: the open findings are tick boxes, not a list as well.
	signOff := state(inbox.StatusInReview)
	signOff.Implementation = &itemstate.Implementation{HeadSHA: "3f9a2c1d00"}
	signOff.Findings = &itemstate.FindingsView{SendBack: "request-changes", Open: []itemstate.FindingView{{ID: "f1", Severity: "blocking", Text: "no size limit", File: "a.php"}}}
	signOff.Actions = []string{"mark-done", "request-changes", "second-opinion", "reject"}
	env = Build(Input{State: signOff, Actions: true})
	conforms(t, env)
	boxes := byKind(t, env)["CheckBox"]
	if len(boxes) != 1 || boxes[0]["value"].(map[string]any)["@path"] != "/findings/f1" || boxes[0]["label"] != "blocking: no size limit (a.php)" {
		t.Fatalf("the findings: %v", boxes)
	}
	if strings.Contains(strings.Join(texts(t, env), "\n"), "- **blocking** no size limit") {
		t.Fatal("the findings are listed twice")
	}
	if got := buttons(t, env); fmt.Sprint(got) != "[mark-done/primary request-changes/default second-opinion/default reject/borderless]" {
		t.Fatalf("sign-off: %v", got)
	}

	// Not started: send it to the agents; no notes field, no reject.
	open := state(inbox.StatusOpen)
	open.Actions = []string{"queue"}
	env = Build(Input{State: open, Actions: true})
	conforms(t, env)
	if got := buttons(t, env); fmt.Sprint(got) != "[queue/primary]" || len(byKind(t, env)["TextField"]) != 0 {
		t.Fatalf("not started: %v", byKind(t, env))
	}

	for name, in := range map[string]Input{
		"no actions":         {State: gate},
		"a key without them": {State: func() *itemstate.State { s := *gate; s.Actions = []string{}; return &s }(), Actions: true},
	} {
		env := Build(in)
		if k := byKind(t, env); len(k["TextField"])+len(k["Button"]) != 0 || dataModel(env) != nil {
			t.Errorf("%s: inputs on the card: %v", name, k)
		}
		if !strings.Contains(strings.Join(texts(t, env), "\n"), "(a) only on submit") {
			t.Errorf("%s: the questions are text", name)
		}
	}
}

// Findings are tick boxes only when they can go back now, with an action the
// key may take: a retry that only reruns QA cannot send them.
func TestTickableFindings(t *testing.T) {
	st := state(inbox.StatusBlocked)
	st.Actions = []string{"retry", "answers", "reject"}
	st.Findings = &itemstate.FindingsView{Open: []itemstate.FindingView{{ID: "f1", Severity: "blocking", Text: "x"}}}
	for sendBack, want := range map[string]bool{"retry": true, "": false, "request-changes": false} {
		st.Findings.SendBack = sendBack
		if got := tickableFindings(st); got != want {
			t.Errorf("send_back %q: %v", sendBack, got)
		}
		env := Build(Input{State: st, Actions: true})
		if boxes := len(byKind(t, env)["CheckBox"]); (boxes == 1) != want {
			t.Errorf("send_back %q: %d tick boxes", sendBack, boxes)
		}
		if listed := strings.Contains(strings.Join(texts(t, env), "\n"), "- **blocking** x"); listed == want {
			t.Errorf("send_back %q: listed as text %v", sendBack, listed)
		}
	}
}

// A refusal reads as a sentence for the person who pressed the button.
func TestForPerson(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w: say why the item is rejected", ErrInvalid):           "Say why the item is rejected",
		fmt.Errorf("%w: question 1 is answered", inbox.ErrInvalidInput):      "Question 1 is answered",
		fmt.Errorf("%w: acting on a card needs …", auth.ErrApproverRequired): "Acting on a card needs …",
	} {
		if got := forPerson(err); got != want {
			t.Errorf("%v: %q", err, got)
		}
	}
}

// Pull requests are named in text, without links.
func TestPRName(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acme/app/pull/7":                 "pull request #7",
		"https://gitlab.example.com/a/b/-/merge_requests/12": "pull request #12",
		"https://example.com/review/abc":                     "pull request",
	} {
		if got := prName(url); got != want {
			t.Errorf("%s: %q", url, got)
		}
	}
}

// At the plan gate the card holds the round as the item does: the comparison
// first, then each plan in full under its number (the synthesis named as the
// merge), with the questions its planner asked; a revision the item holds no
// text for shows its steps.
func TestFullPlans(t *testing.T) {
	gate := state(inbox.StatusAwaitingApproval)
	gate.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{
		revision("r1", "claude", "A"), revision("r2", "codex", "B"), revision("r3", "synthesis", "C"), revision("r4", "glm", "Only steps")}}
	round := &inbox.PlansView{Round: 2, Rounds: 2, Plans: []inbox.PlanDoc{
		{Kind: "candidate", Label: "claude", RevisionID: "r1", Markdown: "# Approach\nKeep the report.\n\n## Steps\n1. A\n\n```\n# not a heading\n```",
			QuestionsMarkdown: "1. Drafts too?"},
		{Kind: "candidate", Label: "codex", RevisionID: "r2", Markdown: "**Revision:** r2\n\nRewrite the report."},
		{Kind: "synthesis", Label: "synthesis", RevisionID: "r3", Markdown: "Both, merged."},
	}, Comparison: &inbox.PlanNote{Heading: "Comparison — judge", Markdown: "## Verdict\nPlan 1 is safer."}}
	env := Build(Input{State: gate, Plans: round})
	conforms(t, env)
	all := strings.Join(texts(t, env), "\n----\n")
	for _, want := range []string{
		"planning round 2",
		"#### Comparison", "##### Verdict\nPlan 1 is safer.",
		"##### Approach\nKeep the report.\n\n###### Steps", "```\n# not a heading\n```",
		"##### Questions this planner asked", "1. Drafts too?",
		"Rewrite the report.", "Both, merged.",
		"1. **Only steps**",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no Text holds %q:\n%s", want, all)
		}
	}
	tabs := byKind(t, env)["Tabs"][0]["tabs"].([]any)
	var titles []string
	for _, tb := range tabs {
		titles = append(titles, tb.(map[string]any)["title"].(string))
	}
	if fmt.Sprint(titles) != "[Plan 1 — claude Plan 2 — codex Plan 3 — merged from the others Plan 4 — glm]" {
		t.Fatalf("the tabs: %v", titles)
	}
	if strings.Contains(all, "**Revision:**") {
		t.Fatal("the revision line is the item's bookkeeping, not the plan")
	}
	if strings.Index(all, "#### Comparison") > strings.Index(all, "Keep the report.") {
		t.Fatal("the comparison comes before the plans")
	}

	gate.Actions = []string{"approve", "request-changes", "reject"}
	picker := byKind(t, Build(Input{State: gate, Plans: round, Actions: true}))["ChoicePicker"][0]["options"].([]any)
	if picker[2].(map[string]any)["label"] != "Plan 3 — merged from the others" {
		t.Fatalf("the picker names the plans as the tabs do: %v", picker)
	}

	long := strings.Repeat("word ", maxPlanText)
	if got := fullText(long); len(got) > maxPlanText+100 || !strings.Contains(got, "The rest is in skopos") {
		t.Fatalf("a long plan is cut and says so: %d", len(got))
	}
	if got := nestHeadings("#### Deep\ntext", 5); got != "##### Deep\ntext" {
		t.Fatalf("headings move below the card's: %q", got)
	}
}

func TestKey(t *testing.T) {
	gate := func(revs ...string) *itemstate.State {
		st := state(inbox.StatusAwaitingApproval)
		st.Plan = &itemstate.PlanView{}
		for _, r := range revs {
			st.Plan.Revisions = append(st.Plan.Revisions, revision(r, "", "x"))
		}
		return st
	}
	a, b := Build(Input{State: gate("r1")}), Build(Input{State: gate("r1")})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("the same state builds the same envelope")
	}
	if Build(Input{State: gate("r2")}).Key == a.Key {
		t.Fatal("a new plan round is a new key")
	}
	withQ := gate("r1")
	withQ.Questions = []itemstate.QuestionView{{ID: "q", SetID: "s9", Text: "?"}}
	if Build(Input{State: withQ}).Key == a.Key {
		t.Fatal("a new question set is a new key")
	}
	w1, w2 := state(inbox.StatusPlanning), state(inbox.StatusImplementing)
	w1.Run = &itemstate.RunView{Stage: runs.StagePlan, Status: "running"}
	w2.Run = &itemstate.RunView{Stage: runs.StageImplement, Status: "running"}
	if Build(Input{State: w1}).Key != Build(Input{State: w2}).Key {
		t.Fatal("the agents' work is one key from start to the next gate")
	}
	b1, b2 := state(inbox.StatusBlocked), state(inbox.StatusBlocked)
	b1.Reason, b2.Reason = "checks failed: lint", "checks failed: test"
	if Build(Input{State: b1}).Key == Build(Input{State: b2}).Key {
		t.Fatal("another reason to stop is a new key")
	}
	other := gate("r1")
	other.Item.ID = "01other"
	if Build(Input{State: other}).Key == a.Key {
		t.Fatal("another item is a new key")
	}
	if !strings.HasPrefix(Build(Input{State: gate("r1"), PublicURL: "https://skopos.example"}).URL, "https://skopos.example/?item=01item") {
		t.Fatal("the link to the item")
	}
}

type fakeItems struct {
	items   []inbox.Item
	got     inbox.ListFilter
	content string
	reads   int
}

func (f *fakeItems) ListItemsFilter(_ context.Context, fl inbox.ListFilter) ([]inbox.Item, error) {
	f.got = fl
	return f.items, nil
}

func (f *fakeItems) ReadItem(_ context.Context, id string) (*inbox.Item, error) {
	f.reads++
	return &inbox.Item{ID: id, Content: f.content}, nil
}

type fakeStates struct {
	asked string
	state *itemstate.State // nil = a queued item
}

func (f *fakeStates) Get(_ context.Context, id string) (*itemstate.State, error) {
	f.asked = id
	st := f.state
	if st == nil {
		st = state(inbox.StatusQueued)
	}
	st.Item.ID = id
	return st, nil
}

// The card shows the newest item not closed, else the newest; the
// request is checked before anything is read.
func TestRender(t *testing.T) {
	items := &fakeItems{items: []inbox.Item{
		{ID: "01a", Status: inbox.StatusDone},
		{ID: "01c", Status: inbox.StatusDiscarded},
		{ID: "01b", Status: inbox.StatusQueued},
	}}
	states := &fakeStates{}
	svc := &Service{Items: items, States: states}
	env, err := svc.Render(context.Background(), Request{Subject: Subject{Ref: " JOB-1 "}}, "github.com/acme/app")
	if err != nil || states.asked != "01b" || env.Status != StatusAgent {
		t.Fatalf("the open one: %v %q %+v", err, states.asked, env)
	}
	if items.got.Ref != "JOB-1" || items.got.WorkspaceID != "github.com/acme/app" {
		t.Fatalf("lookup: %+v", items.got)
	}
	items.items = []inbox.Item{{ID: "01a", Status: inbox.StatusDone}, {ID: "01c", Status: inbox.StatusDiscarded}}
	if _, err := svc.Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}}, ""); err != nil || states.asked != "01c" {
		t.Fatalf("all closed: the newest: %v %q", err, states.asked)
	}
	items.items = nil
	if env, err := svc.Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}}, ""); err != nil || env.Status != StatusNone {
		t.Fatalf("no item: %v %+v", err, env)
	}
	if items.reads != 0 {
		t.Fatalf("the item's text is read only at the plan gate or once closed: %d reads", items.reads)
	}
	for _, req := range []Request{
		{Subject: Subject{Ref: "  "}},
		{Subject: Subject{Ref: strings.Repeat("x", 256)}},
		{Subject: Subject{Ref: "J", Text: strings.Repeat("x", maxSubjectText+1)}},
		{Subject: Subject{Ref: "J"}, Action: json.RawMessage(`{"name":"approve"}`)},
	} {
		if _, err := svc.Render(context.Background(), req, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("refused: %+v → %v", req.Subject.Ref, err)
		}
	}
	if _, err := svc.Render(context.Background(), Request{Subject: Subject{Ref: "J"}, Action: json.RawMessage(`null`)}, ""); err != nil {
		t.Fatalf("a null action is none: %v", err)
	}
}

// At the plan gate, "nothing to do" follows the round as the approval reads
// it: a panel round where one planner wrote a plan shows the plan.
func TestRenderNothingToDo(t *testing.T) {
	gate := state(inbox.StatusAwaitingApproval)
	gate.Plan = &itemstate.PlanView{Revisions: []itemstate.RevisionView{revision("r2", "codex", "Do it")}}
	nothing := func(label, why string) string {
		return "\n## Candidate — " + label + "\n\n**Nothing to do.** " + why + "\n"
	}
	plan := "\n## Candidate — codex\n\n**Candidate:** codex · **Revision:** 01a1d342-e9d3-7036-bbef-16902557fb86\n\nA plan.\n"
	for _, tc := range []struct{ name, content, want, not string }{
		{"one planner wrote a plan", "Brief." + nothing("claude", "Nothing changed.") + plan, "1. **Do it**", "found nothing to do"},
		{"every planner found nothing", "Brief." + nothing("claude", "Nothing changed.") + nothing("codex", "Same."), "Nothing changed.", "**Do it**"},
	} {
		items := &fakeItems{items: []inbox.Item{{ID: "01g", Status: inbox.StatusAwaitingApproval}}, content: tc.content}
		svc := &Service{Items: items, States: &fakeStates{state: gate}}
		env, err := svc.Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}}, "")
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(texts(t, env), "\n")
		if !strings.Contains(all, tc.want) || strings.Contains(all, tc.not) {
			t.Errorf("%s: want %q, not %q:\n%s", tc.name, tc.want, tc.not, all)
		}
	}
}

type fakeActors struct {
	name     string
	item     string
	answers  qa.AnswersInput
	notes    string
	findings []string
	approve  inbox.ApproveInput
	head     string
	err      error // what the second opinion answers
}

func (f *fakeActors) did(name, item string) (*inbox.Item, error) {
	f.name, f.item = name, item
	return &inbox.Item{ID: item}, nil
}
func (f *fakeActors) Queue(_ context.Context, id, _ string) (*inbox.Item, error) {
	return f.did(ActionQueue, id)
}
func (f *fakeActors) ReviewBranch(_ context.Context, id, _ string) (*inbox.Item, error) {
	return f.did(ActionReview, id)
}
func (f *fakeActors) Approve(_ context.Context, id string, in inbox.ApproveInput) (*inbox.Item, error) {
	f.approve, f.answers, f.notes = in, in.Answers, in.Notes
	return f.did(ActionApprove, id)
}
func (f *fakeActors) RequestChangesWith(_ context.Context, id string, in inbox.ChangesInput) (*inbox.Item, error) {
	f.answers, f.notes, f.findings = in.Answers, in.Notes, in.Findings
	return f.did(ActionRequestChanges, id)
}
func (f *fakeActors) Answer(_ context.Context, id string, answers qa.AnswersInput, _ string) (*inbox.Item, error) {
	f.answers = answers
	return f.did(ActionAnswer, id)
}
func (f *fakeActors) RetryOn(_ context.Context, id string, in inbox.RetryInput) (*inbox.Item, error) {
	f.answers, f.findings = in.Answers, in.Findings
	return f.did(ActionRetry, id)
}
func (f *fakeActors) MarkDone(_ context.Context, id, head, _ string) (*inbox.Item, error) {
	f.head = head
	return f.did(ActionMarkDone, id)
}
func (f *fakeActors) CheckCommits(_ context.Context, id, _ string) (*inbox.Item, error) {
	return f.did(ActionCheckCommits, id)
}
func (f *fakeActors) RequestSecondOpinion(_ context.Context, id string, _ inbox.SecondOpinionInput) (*inbox.SecondOpinionRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.name, f.item = ActionSecondOpinion, id
	return &inbox.SecondOpinionRequest{}, nil
}
func (f *fakeActors) Reject(_ context.Context, id, notes, _ string) (*inbox.Item, error) {
	f.notes = notes
	return f.did(ActionReject, id)
}

// A card's action is carried out on the card's item with what the person
// gave, and only while the card is still the card (the same item and key)
// and the state allows it.
func TestAct(t *testing.T) {
	run := func(st *itemstate.State, status inbox.Status, action string) (*fakeActors, error) {
		t.Helper()
		actors := &fakeActors{}
		svc := &Service{Items: &fakeItems{items: []inbox.Item{{ID: "01item", Status: status}}}, States: &fakeStates{state: st}, Actors: actors}
		_, err := svc.Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}, Action: json.RawMessage(action)}, "")
		return actors, err
	}
	act := func(name, key, rest string) string {
		return `{"name":"` + name + `","context":{"item":"01item","key":"` + key + `"` + rest + `}}`
	}

	gate := state(inbox.StatusAwaitingApproval)
	gate.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{revision("r1", "claude", "A")}}
	gate.Plan.Revisions[0].ContentHash = "hash-r1"
	gate.Questions = []itemstate.QuestionView{{ID: "q1", SetID: "s1", N: 1, Asker: "planner", Text: "Drafts?", Options: []string{"yes", "no"}}}
	gate.Actions = []string{"approve", "request-changes", "answers", "reject"}
	gateKey := Build(Input{State: gate}).Key

	// Approve: the plan's hash from the state, the answers and the note
	// with it; the A2UI message around the action is taken too.
	a, err := run(gate, inbox.StatusAwaitingApproval, `{"version":"1.0","action":`+act("approve", gateKey,
		`,"answers":{"q1":{"option":["b"],"text":""}},"notes":" keep it small ","plan":[]`)+`}`)
	if err != nil || a.name != ActionApprove || a.approve.RevisionID != "r1" || a.approve.ContentHash != "hash-r1" || a.notes != "keep it small" ||
		len(a.answers.Items) != 1 || a.answers.Items[0] != (qa.AnswerItem{QuestionID: "q1", Option: "b"}) {
		t.Fatalf("approve: %v %+v", err, a)
	}

	for name, tc := range map[string]struct {
		action string
		want   error
	}{
		"another item":              {`{"name":"approve","context":{"item":"02item","key":"` + gateKey + `"}}`, ErrStale},
		"the item moved on":         {act("approve", "01item:agent", ""), ErrStale},
		"not possible now":          {act("mark-done", gateKey, ""), ErrStale},
		"a plan no longer there":    {act("approve", gateKey, `,"plan":["r9"]`), ErrStale},
		"changes without a word":    {act("request-changes", gateKey, `,"notes":"  "`), ErrInvalid},
		"reject without a reason":   {act("reject", gateKey, ""), ErrInvalid},
		"answers without an answer": {act("answers", gateKey, `,"answers":{"q1":{"option":[],"text":""}}`), ErrInvalid},
		"an unknown action":         {act("delete", gateKey, ""), ErrInvalid},
		"not an action":             {`"approve"`, ErrInvalid},
	} {
		if a, err := run(gate, inbox.StatusAwaitingApproval, tc.action); !errors.Is(err, tc.want) || a.name != "" {
			t.Errorf("%s: %v (carried out: %q)", name, err, a.name)
		}
	}
	if a, err := run(gate, inbox.StatusAwaitingApproval, act("reject", gateKey, `,"notes":"not needed"`)); err != nil || a.name != ActionReject || a.notes != "not needed" {
		t.Fatalf("reject: %v %+v", err, a)
	}

	// Several plans: one must be picked.
	panel := state(inbox.StatusAwaitingApproval)
	panel.Plan = &itemstate.PlanView{PlanID: "p", Revisions: []itemstate.RevisionView{revision("r1", "claude", "A"), revision("r2", "codex", "B")}}
	panel.Actions = gate.Actions
	panelKey := Build(Input{State: panel}).Key
	if _, err := run(panel, inbox.StatusAwaitingApproval, act("approve", panelKey, "")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("several plans, none picked: %v", err)
	}
	if a, err := run(panel, inbox.StatusAwaitingApproval, act("approve", panelKey, `,"plan":["r2"]`)); err != nil || a.approve.RevisionID != "r2" {
		t.Fatalf("the picked plan: %v %+v", err, a)
	}

	// A stopped gate: the note is an answer to the stop; ticked findings go
	// back with the retry.
	blocked := state(inbox.StatusBlocked)
	blocked.Reason = "review blocked"
	blocked.Findings = &itemstate.FindingsView{Open: []itemstate.FindingView{{ID: "f1", Severity: "blocking", Text: "x"}, {ID: "f2", Severity: "nit", Text: "y"}}}
	blocked.Actions = []string{"retry", "answers", "reject"}
	blockedKey := Build(Input{State: blocked}).Key
	if a, err := run(blocked, inbox.StatusBlocked, act("retry", blockedKey, `,"notes":"use the staging data","findings":{"f1":true,"f2":false}`)); err != nil ||
		a.name != ActionRetry || a.answers.Text != "use the staging data" || fmt.Sprint(a.findings) != "[f1]" {
		t.Fatalf("retry: %v %+v", err, a)
	}

	// Sign-off: the reviewed commit is the state's.
	signOff := state(inbox.StatusInReview)
	signOff.Implementation = &itemstate.Implementation{HeadSHA: "3f9a2c1d00"}
	signOff.Actions = []string{"mark-done", "request-changes", "reject"}
	signOffKey := Build(Input{State: signOff}).Key
	if a, err := run(signOff, inbox.StatusInReview, act("mark-done", signOffKey, "")); err != nil || a.head != "3f9a2c1d00" {
		t.Fatalf("sign off: %v %+v", err, a)
	}

	// The other actions reach their own inbox action; a card drawn before
	// the item entered its status (a new stop, a new stage) is out of date.
	open := state(inbox.StatusOpen)
	open.Item.Branch = "agent/x"
	open.Actions = []string{"queue", "review"}
	openKey := Build(Input{State: open}).Key
	for _, name := range []string{ActionQueue, ActionReview} {
		if a, err := run(open, inbox.StatusOpen, act(name, openKey, "")); err != nil || a.name != name {
			t.Errorf("%s: %v %+v", name, err, a)
		}
	}
	signOff.Actions = []string{"mark-done", "request-changes", "check-commits", "second-opinion", "reject"}
	for _, name := range []string{ActionCheckCommits, ActionSecondOpinion} {
		if a, err := run(signOff, inbox.StatusInReview, act(name, signOffKey, "")); err != nil || a.name != name || a.item != "01item" {
			t.Errorf("%s: %v %+v", name, err, a)
		}
	}
	stopped := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	blocked.Item.StatusSince = &stopped
	if a, err := run(blocked, inbox.StatusBlocked, act("retry", blockedKey, `,"since":"2026-11-20T09:00:00Z"`)); !errors.Is(err, ErrStale) || a.name != "" {
		t.Fatalf("a card from an earlier stop with the same reason: %v %+v", err, a)
	}
	if _, err := run(blocked, inbox.StatusBlocked, act("retry", blockedKey, `,"since":"2026-11-20T10:00:00Z"`)); err != nil {
		t.Fatalf("the card of this stop: %v", err)
	}

	reader := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k1", Capabilities: map[string]struct{}{"rest": {}, "inbox:read": {}}})
	actors := &fakeActors{}
	svc := &Service{Items: &fakeItems{items: []inbox.Item{{ID: "01item", Status: inbox.StatusAwaitingApproval}}}, States: &fakeStates{state: gate}, Actors: actors}
	if _, err := svc.Render(reader, Request{Subject: Subject{Ref: "JOB-1"}, Action: json.RawMessage(act("approve", gateKey, ""))}, ""); !errors.Is(err, auth.ErrApproverRequired) || actors.name != "" {
		t.Fatalf("a key that may not act: %v", err)
	}
	if env, _ := svc.Render(reader, Request{Subject: Subject{Ref: "JOB-1"}}, ""); len(byKind(t, env)["Button"]) != 0 {
		t.Fatal("a key that may not act gets no buttons")
	}
}

// defaultRunner is the inbox's answer to which runner a second opinion that
// names none would use.
type defaultRunner struct {
	*fakeActors
	runner string
}

func (d defaultRunner) SecondOpinionRunner(context.Context, string) string { return d.runner }

// A card names no runner: it offers a second opinion only where the
// workspace has a default one, and the inbox's refusal of one is a 409
// with its reason.
func TestSecondOpinionFromACard(t *testing.T) {
	st := state(inbox.StatusInReview)
	st.Implementation = &itemstate.Implementation{HeadSHA: "3f9a2c1d00"}
	st.Actions = []string{"mark-done", "second-opinion", "reject"}
	cardKey := Build(Input{State: st}).Key
	action := json.RawMessage(`{"name":"second-opinion","context":{"item":"01item","key":"` + cardKey + `"}}`)
	svc := func(runner string, err error) *Service {
		items := &fakeItems{items: []inbox.Item{{ID: "01item", Status: inbox.StatusInReview}}}
		clone := *st
		clone.Actions = slices.Clone(st.Actions)
		return &Service{Items: items, States: &fakeStates{state: &clone}, Actors: defaultRunner{&fakeActors{err: err}, runner}}
	}

	env, err := svc("", nil).Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}}, "")
	if err != nil || strings.Contains(strings.Join(texts(t, env), "\n"), "Ask for a second opinion") {
		t.Fatalf("no default runner, no button: %v", err)
	}
	if _, err := svc("", nil).Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}, Action: action}, ""); !errors.Is(err, ErrStale) {
		t.Fatalf("no default runner, the action is refused: %v", err)
	}
	env, err = svc("codex", nil).Render(context.Background(), Request{Subject: Subject{Ref: "JOB-1"}}, "")
	if err != nil || !strings.Contains(strings.Join(texts(t, env), "\n"), "Ask for a second opinion") {
		t.Fatalf("a default runner, the button: %v", err)
	}

	conflict := fmt.Errorf("%w: codex already reviewed head 3f9a2c1", inbox.ErrSecondOpinionConflict)
	h := NewHandler(svc("codex", conflict))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/inbox/card?ref=JOB-1", strings.NewReader(`{"action":`+string(action)+`}`))
	req.Header.Set("Content-Type", "application/json")
	h.Post(rec, req)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Codex already reviewed head 3f9a2c1") {
		t.Fatalf("the inbox's refusal: %d %s", rec.Code, rec.Body.String())
	}
}

// The body is optional with the reference in the address, also when its
// length is not known (chunked).
func TestEmptyChunkedBody(t *testing.T) {
	h := NewHandler(&Service{Items: &fakeItems{}, States: &fakeStates{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/inbox/card?ref=JOB-1", strings.NewReader(""))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	h.Post(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"none"`) {
		t.Fatalf("an empty chunked body: %d %s", rec.Code, rec.Body.String())
	}
}

// Plan text on a card: code blocks of either fence are left alone, a cut
// inside one closes it, and only the bookkeeping revision line goes.
func TestPlanTextCode(t *testing.T) {
	if got := nestHeadings("## Steps\n~~~sh\n# install deps\n~~~", 5); got != "##### Steps\n~~~sh\n# install deps\n~~~" {
		t.Fatalf("a ~~~ block keeps its comment lines: %q", got)
	}
	if got := nestHeadings("## A\n~~~\n```\n# x\n~~~\n## B", 5); got != "##### A\n~~~\n```\n# x\n~~~\n##### B" {
		t.Fatalf("a block ends only at its own fence: %q", got)
	}
	long := "Intro\n```go\n" + strings.Repeat("x := 1\n", maxPlanText)
	got := fullText(long)
	if !strings.HasSuffix(got, "```\n\n_The rest is in skopos (Open)._") {
		t.Fatalf("a cut inside a code block closes it: %q", got[len(got)-80:])
	}
	if got := withoutRevisionLine("**Revision:** r1\n\n1. Set **Revision:** in the form\n**Revision:** kept"); got != "\n1. Set **Revision:** in the form\n**Revision:** kept" {
		t.Fatalf("only the first revision line goes: %q", got)
	}
}
