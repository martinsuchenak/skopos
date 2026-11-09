package qa

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
	"github.com/martinsuchenak/skopos/internal/itemtext"
)

// AuditRecorder records the question.* events (internal/audit).
type AuditRecorder interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// Service records and reads questions and answers. Callers check the item's
// scope first (the inbox service and the run handlers do).
type Service struct {
	storage *Storage
	audit   AuditRecorder
	now     func() time.Time
}

func NewService(storage *Storage) *Service { return &Service{storage: storage, now: time.Now} }

func (s *Service) SetAuditRecorder(a AuditRecorder) { s.audit = a }

// AskInput is one asking: who asked, about what, and the questions.
type AskInput struct {
	WorkspaceID string
	ItemID      string
	RunID       string
	Asker       string
	Stage       string
	Runner      string
	Label       string
	SubjectKind string
	Subject     string
	Blocking    bool
	Source      string
	Questions   []itemtext.Question
	// At: when it was asked (imports keep the order); zero = now.
	At time.Time
}

// Ask records a set and returns it; nil when there are no questions. Each
// question keeps its position as its number (the number steps cite); an
// empty one is kept as "(no question)" so the numbers hold.
func (s *Service) Ask(ctx context.Context, in AskInput) (*Set, error) {
	in.ItemID, in.WorkspaceID = strings.TrimSpace(in.ItemID), strings.TrimSpace(in.WorkspaceID)
	if in.ItemID == "" || in.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: item and workspace are required", ErrInvalidInput)
	}
	switch in.Asker {
	case AskerPlanner, AskerImplementer, AskerWorker:
	default:
		return nil, fmt.Errorf("%w: asker must be planner, implementer or worker", ErrInvalidInput)
	}
	switch in.SubjectKind {
	case SubjectPlanRevision, SubjectBase, SubjectHead:
	case "":
		in.SubjectKind = SubjectNone
	case SubjectNone:
	default:
		return nil, fmt.Errorf("%w: unknown subject_kind %q", ErrInvalidInput, in.SubjectKind)
	}
	if in.Source == "" {
		in.Source = SourceWorker
	}
	hasAny := false
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Text) != "" {
			hasAny = true
		}
	}
	if !hasAny {
		return nil, nil
	}
	at := in.At
	if at.IsZero() {
		at = s.now()
	}
	set := Set{
		ID: ids.New(), WorkspaceID: in.WorkspaceID, ItemID: in.ItemID, Asker: in.Asker, Stage: in.Stage,
		RunID: in.RunID, Runner: in.Runner, Label: strings.TrimSpace(in.Label),
		SubjectKind: in.SubjectKind, Subject: strings.TrimSpace(in.Subject), Blocking: in.Blocking,
		Source: in.Source, CreatedAt: at.UTC(),
	}
	for i, q := range in.Questions {
		text := strings.Join(strings.Fields(q.Text), " ")
		if text == "" {
			text = "(no question)"
		}
		var opts []string
		for _, o := range q.Options {
			if o = strings.Join(strings.Fields(o), " "); o != "" {
				opts = append(opts, o)
			}
		}
		if opts == nil {
			opts = []string{}
		}
		set.Questions = append(set.Questions, Question{ID: ids.New(), SetID: set.ID, N: i + 1, Text: text, Options: opts, State: StateOpen})
	}
	if err := s.storage.InsertSet(ctx, set); err != nil {
		return nil, err
	}
	if in.Source != SourceImported {
		s.recordedLive(ctx, set.ItemID)
	}
	set.Open = true
	if s.audit != nil && in.Source != SourceImported {
		var lines []string
		for _, q := range set.Questions {
			lines = append(lines, fmt.Sprintf("%d. %s", q.N, q.Text))
		}
		agent := ""
		if in.RunID != "" {
			agent = in.Asker + " (run " + in.RunID + ")"
		}
		_ = s.audit.Record(ctx, audit.RecordInput{
			WorkspaceID: set.WorkspaceID, EntityType: "inbox_item", EntityID: set.ItemID, Action: "question.asked",
			Via: audit.ViaWorker, AgentID: agent,
			Notes: fmt.Sprintf("%s asked (set %s, %s %s):\n%s", askedBy(set), set.ID, set.SubjectKind, set.Subject, strings.Join(lines, "\n")),
		})
	}
	return &set, nil
}

// recordedLive marks an item as past the import: its markdown is now
// written from the record, so a later start must not replay it.
func (s *Service) recordedLive(ctx context.Context, itemID string) {
	_ = s.storage.SetImportVersion(ctx, itemID, importVersion)
}

func askedBy(s Set) string {
	if a := s.Asked(); a != "" {
		return a
	}
	return s.Asker
}

// Render is a set as its item section's body: itemtext's numbered list with
// lettered options.
func Render(set Set) string {
	qs := make([]itemtext.Question, len(set.Questions))
	for i, q := range set.Questions {
		qs[i] = itemtext.Question{Text: q.Text, Options: q.Options}
	}
	return itemtext.FormatQuestions(qs)
}

// Item returns an item's sets in order, each question with its state and
// answers.
func (s *Service) Item(ctx context.Context, itemID string) ([]Set, error) {
	sets, err := s.storage.ItemSets(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	answers, err := s.storage.ItemAnswers(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if sets == nil {
		sets = []Set{}
	}
	return withState(sets, answers), nil
}

// Record is the item's sets (Item) and its free answers.
func (s *Service) Record(ctx context.Context, itemID string) (*Record, error) {
	sets, err := s.storage.ItemSets(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	answers, err := s.storage.ItemAnswers(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if sets == nil {
		sets = []Set{}
	}
	r := &Record{Sets: withState(sets, answers)}
	for _, a := range answers {
		if a.Kind == KindGeneral && a.SetID == "" {
			r.Free = append(r.Free, a)
		}
	}
	return r, nil
}

// Prepared is an action's answers, checked against the item's open
// questions before the action runs: the rows to write once it succeeded
// (Commit), the "## Answers" section to append with it, and what free
// text was matched to.
type Prepared struct {
	ItemID      string
	WorkspaceID string
	Action      string
	Markdown    string
	Matches     []Match
	rows        []Answer
}

// Prepare reads an action's answers against the item's open questions.
// Items name questions by id: each must be the item's and open, an option
// one of its letters. Text is matched line by line (matchText); what
// matches nothing is a general answer to the open sets. With no open
// questions the text is kept as it is (Markdown) and no rows are written.
// gate: the action passes a gate (approve, request changes, retry), so the
// open questions it leaves unanswered are recorded as skipped.
func (s *Service) Prepare(ctx context.Context, itemID, workspaceID, action string, in AnswersInput, gate bool) (*Prepared, error) {
	sets, err := s.Item(ctx, itemID)
	if err != nil {
		return nil, err
	}
	open := openSets(sets)
	p := &Prepared{ItemID: itemID, WorkspaceID: workspaceID, Action: action}
	var linked []Answer
	general := map[string]string{}
	if len(in.Items) > 0 {
		byID := map[string]struct {
			q   Question
			set Set
		}{}
		for _, st := range sets {
			for _, q := range st.Questions {
				byID[q.ID] = struct {
					q   Question
					set Set
				}{q, st}
			}
		}
		seen := map[string]bool{}
		for _, it := range in.Items {
			e, ok := byID[strings.TrimSpace(it.QuestionID)]
			if !ok {
				return nil, fmt.Errorf("%w: question %s is not one of this item's", ErrInvalidInput, it.QuestionID)
			}
			replaces := strings.TrimSpace(it.Replaces)
			switch cur := current(e.q); {
			case e.q.State == StateSuperseded:
				return nil, fmt.Errorf("%w: question %d (%s) is superseded: a later round asked again", ErrInvalidInput, e.q.N, e.q.ID)
			case cur != nil && replaces == "":
				return nil, fmt.Errorf("%w: question %d is answered (%s); a correction names the answer it replaces", ErrInvalidInput, e.q.N, cur.ID)
			case cur != nil && replaces != cur.ID:
				return nil, fmt.Errorf("%w: question %d's current answer is %s, not %s", ErrInvalidInput, e.q.N, cur.ID, replaces)
			case cur == nil && replaces != "":
				return nil, fmt.Errorf("%w: question %d has no answer to replace", ErrInvalidInput, e.q.N)
			}
			if seen[e.q.ID] {
				return nil, fmt.Errorf("%w: question %d is answered twice", ErrInvalidInput, e.q.N)
			}
			seen[e.q.ID] = true
			opt := strings.ToLower(strings.TrimSpace(it.Option))
			if opt != "" && (len(opt) != 1 || int(opt[0]-'a') >= len(e.q.Options) || opt[0] < 'a') {
				return nil, fmt.Errorf("%w: question %d has no option %q", ErrInvalidInput, e.q.N, it.Option)
			}
			text := strings.TrimSpace(it.Text)
			if opt == "" && text == "" {
				continue // left blank: not an answer
			}
			linked = append(linked, Answer{Kind: KindAnswer, QuestionID: e.q.ID, SetID: e.set.ID, Option: opt, Text: text, Replaces: replaces})
			p.Matches = append(p.Matches, Match{QuestionID: e.q.ID, SetID: e.set.ID, N: e.q.N, Option: opt, Replaces: replaces})
		}
	}
	if t := strings.TrimSpace(in.Text); t != "" {
		if len(open) == 0 {
			// Nothing open: an answer to the blocker, kept as written and
			// recorded as a free answer.
			p.Markdown = t
			p.rows = append(p.rows, Answer{Kind: KindGeneral, Text: t})
		} else {
			m := matchText(open, t, false)
			linked = appendNew(linked, m.answers) // an answer by id wins
			p.Matches = append(p.Matches, m.report...)
			for id, g := range m.general {
				general[id] = g
			}
		}
	}
	// Notes ("approve: 1) a, 2) Redis" from Slack or the CLI): lines that
	// answer open questions are answers; the rest stays notes, never a
	// general answer.
	if t := strings.TrimSpace(in.Notes); t != "" && len(open) > 0 {
		m := matchText(open, t, true)
		linked = appendNew(linked, m.answers)
		p.Matches = append(p.Matches, m.report...)
	}
	answered := map[string]bool{}
	for _, a := range linked {
		answered[a.QuestionID] = true
		p.rows = append(p.rows, a)
	}
	for id, g := range general {
		p.rows = append(p.rows, Answer{Kind: KindGeneral, SetID: id, Text: g})
	}
	if gate {
		for _, st := range open {
			if general[st.ID] != "" {
				continue
			}
			for _, q := range st.Questions {
				if q.State == StateOpen && !answered[q.ID] {
					p.rows = append(p.rows, Answer{Kind: KindSkipped, QuestionID: q.ID, SetID: st.ID})
				}
			}
		}
	}
	if p.Markdown == "" {
		p.Markdown = renderAnswers(sets, linked, general)
	}
	return p, nil
}

// appendNew adds the answers to questions not answered yet.
func appendNew(have, more []Answer) []Answer {
	seen := map[string]bool{}
	for _, a := range have {
		seen[a.QuestionID] = true
	}
	for _, a := range more {
		if !seen[a.QuestionID] {
			seen[a.QuestionID] = true
			have = append(have, a)
		}
	}
	return have
}

// renderAnswers is the "## Answers" section for an action's answers: each
// answered question with its text (numbers repeat across rounds), the
// option's text and the words; then the general answers. Several sets:
// grouped under "To <who>:".
func renderAnswers(sets []Set, linked []Answer, general map[string]string) string {
	var blocks []string
	for _, st := range sets {
		var lines []string
		for _, q := range st.Questions {
			for _, a := range linked {
				if a.QuestionID != q.ID {
					continue
				}
				var parts []string
				if a.Option != "" {
					parts = append(parts, "("+a.Option+") "+q.Options[int(a.Option[0]-'a')])
				}
				if a.Text != "" {
					parts = append(parts, a.Text)
				}
				label := "Answer"
				if a.Replaces != "" {
					label = "Corrected answer"
				}
				lines = append(lines, fmt.Sprintf("%d. %s\n   %s: %s", q.N, q.Text, label, strings.Join(parts, " — ")))
			}
		}
		if g := general[st.ID]; g != "" {
			lines = append(lines, g)
		}
		if len(lines) == 0 {
			continue
		}
		block := strings.Join(lines, "\n")
		if len(sets) > 1 && st.Asked() != "" {
			block = "To " + st.Asked() + ":\n\n" + block
		}
		blocks = append(blocks, block)
	}
	// One general text given to several sets renders once.
	return strings.Join(dedupe(blocks), "\n\n")
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Commit writes a prepared action's rows once the action succeeded: the
// actor from the request's principal, the approval it came with, and one
// audit event per row.
func (s *Service) Commit(ctx context.Context, p *Prepared, approvalID, via string) error {
	if p == nil || len(p.rows) == 0 {
		return nil
	}
	actor, keyID := actorOf(ctx)
	via = cleanVia(ctx, via)
	now := s.now().UTC()
	rows := make([]Answer, len(p.rows))
	for i, a := range p.rows {
		a.ID = ids.New()
		a.WorkspaceID, a.ItemID, a.Action = p.WorkspaceID, p.ItemID, p.Action
		a.ApprovalID, a.Actor, a.ActorKeyID, a.Via = approvalID, actor, keyID, via
		a.CreatedAt = now.Add(time.Duration(i) * time.Millisecond)
		rows[i] = a
	}
	if err := s.storage.InsertAnswers(ctx, rows); err != nil {
		return err
	}
	s.recordedLive(ctx, p.ItemID)
	if s.audit == nil {
		return nil
	}
	for _, a := range rows {
		action, notes := "question.answered", ""
		switch a.Kind {
		case KindAnswer:
			notes = fmt.Sprintf("question %s: %s", a.QuestionID, strings.TrimSpace(optionText(a)+" "+a.Text))
			if a.Replaces != "" {
				action, notes = "question.corrected", notes+" (replaces "+a.Replaces+")"
			}
		case KindGeneral:
			notes = fmt.Sprintf("set %s (free text): %s", a.SetID, a.Text)
			if a.SetID == "" {
				notes = "free answer (no question open): " + a.Text
			}
		case KindSkipped:
			action, notes = "question.skipped", fmt.Sprintf("question %s left unanswered (%s)", a.QuestionID, a.Action)
		}
		_ = s.audit.Record(ctx, audit.RecordInput{
			WorkspaceID: a.WorkspaceID, EntityType: "inbox_item", EntityID: a.ItemID, Action: action, Via: via, Notes: notes,
		})
	}
	return nil
}

func optionText(a Answer) string {
	if a.Option == "" {
		return ""
	}
	return "(" + a.Option + ")"
}

// actorOf renders the principal as the audit log does.
func actorOf(ctx context.Context) (string, string) {
	p := auth.PrincipalFromContext(ctx)
	switch {
	case p == nil:
		return "system", ""
	case p.Root:
		return "root", ""
	default:
		return "key " + p.Name, p.KeyID
	}
}

// cleanVia keeps the closed set of surfaces (audit's), slack only for an
// approver.
func cleanVia(ctx context.Context, via string) string {
	via = strings.TrimSpace(via)
	switch via {
	case audit.ViaDashboard, audit.ViaCLI, audit.ViaMCP, audit.ViaAPI, audit.ViaWorker:
		return via
	case audit.ViaSlack:
		if p := auth.PrincipalFromContext(ctx); p == nil || p.Root || p.Approver {
			return via
		}
	case audit.ViaMigration:
		if auth.PrincipalFromContext(ctx) == nil {
			return via
		}
	}
	return ""
}
