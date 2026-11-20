package cards

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemstate"
	"github.com/martinsuchenak/skopos/internal/plans"
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
		{"queued", Input{State: queued}, StatusAgent, false, "Skopos is working on it: planning", []string{"**Queued:** planning"}},
		{"working", Input{State: working}, StatusAgent, false, "Skopos is working on it: implementing", []string{"implementing (claude)"}},
		{"plan approval", Input{State: gate}, StatusPlanApproval, true, "Plan ready for approval — 2 questions",
			[]string{"1. **Add the action**\\\n   do Add the action\n2. **Test it**", "**Questions — planner claude** (the work waits for the answers)",
				"1. Drafts too?\n   - (a) only on submit\n   - (b) on every save\n2. Preload setting here?"}},
		{"plan approval, two plans", Input{State: panel}, StatusPlanApproval, true, "Plan ready for approval — 2 plans", nil},
		{"plan approval, nothing to do", Input{State: state(inbox.StatusAwaitingApproval), NothingToDo: "already done upstream"}, StatusPlanApproval, true,
			"Waiting for you: the planner found nothing to do — already done upstream", []string{"already done upstream"}},
		{"blocked", Input{State: blocked}, StatusBlocked, true, "Blocked: qa needs an answer: no test form", []string{"more detail", "**Questions — QA**"}},
		{"failed", Input{State: state(inbox.StatusFailed)}, StatusFailed, true, "Failed", nil},
		{"sign-off", Input{State: signOff}, StatusSignOff, true, "Branch ready for sign-off — 1 blocking finding open",
			[]string{"[Pull request](https://github.com/acme/app/pull/7) at `3f9a2c1`", "Checks: passed — lint, test", "Review (claude): **1 blocking**, 2 nits", "- **blocking** no size limit (`a.php`)"}},
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
