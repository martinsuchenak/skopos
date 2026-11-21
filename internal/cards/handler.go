package cards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemstate"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Items finds the items of a reference and reads one whole (both
// scope-checked).
type Items interface {
	ListItemsFilter(ctx context.Context, f inbox.ListFilter) ([]inbox.Item, error)
	ReadItem(ctx context.Context, id string) (*inbox.Item, error)
}

// States builds an item's state (internal/itemstate).
type States interface {
	Get(ctx context.Context, itemID string) (*itemstate.State, error)
}

// Actors carries out what a card's buttons ask: the inbox service's
// workflow actions, each with its own checks.
type Actors interface {
	Queue(ctx context.Context, itemID, via string) (*inbox.Item, error)
	ReviewBranch(ctx context.Context, itemID, via string) (*inbox.Item, error)
	Approve(ctx context.Context, itemID string, in inbox.ApproveInput) (*inbox.Item, error)
	RequestChangesWith(ctx context.Context, itemID string, in inbox.ChangesInput) (*inbox.Item, error)
	Answer(ctx context.Context, itemID string, answers qa.AnswersInput, via string) (*inbox.Item, error)
	RetryOn(ctx context.Context, itemID string, in inbox.RetryInput) (*inbox.Item, error)
	MarkDone(ctx context.Context, itemID, headSHA, via string) (*inbox.Item, error)
	CheckCommits(ctx context.Context, itemID, via string) (*inbox.Item, error)
	RequestSecondOpinion(ctx context.Context, itemID string, in inbox.SecondOpinionInput) (*inbox.SecondOpinionRequest, error)
	Reject(ctx context.Context, itemID, notes, via string) (*inbox.Item, error)
}

// secondOpinionRunner is the runner a second opinion that names none would
// use (the inbox service; "" = none).
type secondOpinionRunner interface {
	SecondOpinionRunner(ctx context.Context, workspaceID string) string
}

// Service builds cards.
type Service struct {
	Items  Items
	States States
	// Actors takes a card's actions (nil = cards take none).
	Actors Actors
	// PublicURL is the server's public address, for the card's link.
	PublicURL string
}

// Request is what a host sends.
type Request struct {
	Subject Subject `json:"subject"`
	// Action is the A2UI action a card's button sent (its event name and
	// resolved context), as the host's renderer gives it; the A2UI message
	// around it ({"version", "action"}) is taken too. With one, the action
	// is carried out first and the answer is the card after it.
	Action json.RawMessage `json:"action,omitempty"`
}

// action is what a card's button sends (actionForm): its name, and the
// context the renderer resolved from the card's inputs.
type action struct {
	Name    string `json:"name"`
	Context struct {
		Item    string `json:"item"`
		Key     string `json:"key"`
		Since   string `json:"since"`
		Answers map[string]struct {
			Option json.RawMessage `json:"option"`
			Text   string          `json:"text"`
		} `json:"answers"`
		Plan     json.RawMessage `json:"plan"`
		Findings map[string]bool `json:"findings"`
		Notes    string          `json:"notes"`
	} `json:"context"`
}

// ErrStale is an action on a card that has changed since it was drawn: the
// item, or its state (the card's key), is no longer what the person saw.
var ErrStale = errors.New("the card has changed since it was shown")

// CanAct: the key asking may act on items for a person (a card's buttons
// answer as the approver the request names): inbox write, and the approver
// permission. The internal caller may.
func CanAct(ctx context.Context) bool {
	p := auth.PrincipalFromContext(ctx)
	if p == nil {
		return true
	}
	return p.Run == nil && p.Allows(auth.AreaInbox, true) && (p.Root || p.Approver)
}

// Limits of a request.
const (
	maxRef         = 255
	maxSubjectText = 64 << 10
)

// ErrInvalid is a request the card endpoint refuses.
var ErrInvalid = errors.New("invalid card request")

// Render is the card of the item with the subject's reference: the newest
// one not closed, else the newest. workspace narrows the lookup to one
// workspace ("" = every workspace the key reaches).
func (s *Service) Render(ctx context.Context, req Request, workspace string) (*Envelope, error) {
	ref := strings.TrimSpace(req.Subject.Ref)
	if ref == "" || len(ref) > maxRef {
		return nil, fmt.Errorf("%w: the reference is required (?ref= or subject.ref), at most %d bytes", ErrInvalid, maxRef)
	}
	if len(req.Subject.Text) > maxSubjectText {
		return nil, fmt.Errorf("%w: subject.text is at most %d bytes", ErrInvalid, maxSubjectText)
	}
	req.Subject.Ref = ref
	if len(req.Action) > 0 && string(req.Action) != "null" {
		if err := s.act(ctx, req, workspace); err != nil {
			return nil, err
		}
	}
	return s.render(ctx, req, workspace)
}

// render is the card of the subject's item as it is now.
func (s *Service) render(ctx context.Context, req Request, workspace string) (*Envelope, error) {
	in, _, err := s.current(ctx, req, workspace)
	if err != nil {
		return nil, err
	}
	return Build(in), nil
}

// current is what the card of the subject's item is built from now, and the
// item (nil = no item has the reference).
func (s *Service) current(ctx context.Context, req Request, workspace string) (Input, *inbox.Item, error) {
	items, err := s.Items.ListItemsFilter(ctx, inbox.ListFilter{WorkspaceID: strings.TrimSpace(workspace), Ref: req.Subject.Ref})
	if err != nil {
		return Input{}, nil, err
	}
	item := pick(items)
	if item == nil {
		return Input{Subject: req.Subject, PublicURL: s.PublicURL}, nil, nil
	}
	st, err := s.States.Get(ctx, item.ID)
	if err != nil {
		return Input{}, nil, err
	}
	in := Input{Subject: req.Subject, State: st, Outcome: item.Outcome, PublicURL: s.PublicURL, Actions: s.Actors != nil && CanAct(ctx)}
	// A card names no runner, so it asks for a second opinion only where
	// the workspace has a default one (a request without one is refused).
	if r, ok := s.Actors.(secondOpinionRunner); ok && slices.Contains(st.Actions, ActionSecondOpinion) && r.SecondOpinionRunner(ctx, item.WorkspaceID) == "" {
		st.Actions = slices.DeleteFunc(slices.Clone(st.Actions), func(a string) bool { return a == ActionSecondOpinion })
	}
	// Whether the newest plan round found nothing to do matters at the plan
	// gate and on a closed item; it is read from the item's own text, by
	// the rule the approval checks.
	switch inbox.Status(st.Item.Status) {
	case inbox.StatusAwaitingApproval, inbox.StatusDone:
		full, err := s.Items.ReadItem(ctx, item.ID)
		if err != nil {
			return Input{}, nil, err
		}
		in.NothingToDo = inbox.NewestRoundNothingToDo(full.Content)
		if st.Item.Status == string(inbox.StatusAwaitingApproval) && st.Plan != nil {
			// The plans in full, as the item holds them: only for a key that
			// reads plans (the state has them), like the revisions' steps.
			if round, err := inbox.PlansOf(item.ID, full.Title, full.Content, 0); err == nil {
				in.Plans = round
			}
		}
	}
	return in, item, nil
}

// act carries out a card's action on the item the card shows, after checking
// that the card the person saw is still the card (the same item, the same
// key: nothing new happened since) and that the state allows the action now.
// Each action is the inbox's own, with its own checks; the request's
// principal is the actor, so a delegate key's person is recorded.
func (s *Service) act(ctx context.Context, req Request, workspace string) error {
	if s.Actors == nil {
		return fmt.Errorf("%w: this server takes no actions on cards", ErrInvalid)
	}
	a, err := parseAction(req.Action)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(buttonOrder, func(b struct{ action, label string }) bool { return b.action == a.Name }) {
		return fmt.Errorf("%w: unknown action %q", ErrInvalid, a.Name)
	}
	if !CanAct(ctx) {
		return fmt.Errorf("%w: acting on a card needs a key with inbox write and the approver permission", auth.ErrApproverRequired)
	}
	in, item, err := s.current(ctx, req, workspace)
	if err != nil {
		return err
	}
	if item == nil || item.ID != a.Context.Item {
		return fmt.Errorf("%w: it shows another item now", ErrStale)
	}
	st := in.State
	if key(st, status(st, in.NothingToDo), in.NothingToDo) != a.Context.Key || since(st) != a.Context.Since {
		return fmt.Errorf("%w: the item moved on", ErrStale)
	}
	if !slices.Contains(st.Actions, a.Name) {
		return fmt.Errorf("%w: %s is not possible on the item now", ErrStale, a.Name)
	}

	answers := qa.AnswersInput{}
	for _, q := range st.Questions {
		given, ok := a.Context.Answers[q.ID]
		if !ok {
			continue
		}
		it := qa.AnswerItem{QuestionID: q.ID, Option: firstOption(given.Option), Text: strings.TrimSpace(given.Text)}
		if it.Option != "" || it.Text != "" {
			answers.Items = append(answers.Items, it)
		}
	}
	notes := strings.TrimSpace(a.Context.Notes)
	var findings []string
	for _, f := range openFindings(st) {
		if a.Context.Findings[f.ID] {
			findings = append(findings, f.ID)
		}
	}
	via := audit.ViaCard

	switch a.Name {
	case ActionQueue:
		_, err = s.Actors.Queue(ctx, item.ID, via)
	case ActionReview:
		_, err = s.Actors.ReviewBranch(ctx, item.ID, via)
	case ActionApprove:
		var rev *itemstate.RevisionView
		if rev, err = planToApprove(st, firstOption(a.Context.Plan)); err == nil {
			_, err = s.Actors.Approve(ctx, item.ID, inbox.ApproveInput{
				Notes: notes, Answers: answers, Via: via, RevisionID: rev.RevisionID, ContentHash: rev.ContentHash,
			})
		}
	case ActionRequestChanges:
		if notes == "" && len(findings) == 0 {
			return fmt.Errorf("%w: say what to change (a note, or findings to send back)", ErrInvalid)
		}
		_, err = s.Actors.RequestChangesWith(ctx, item.ID, inbox.ChangesInput{Notes: notes, Answers: answers, Via: via, Findings: findings})
	case ActionAnswer:
		if len(answers.Items) == 0 {
			return fmt.Errorf("%w: no question was answered", ErrInvalid)
		}
		_, err = s.Actors.Answer(ctx, item.ID, answers, via)
	case ActionRetry:
		// A note is an answer to what stopped the work, as the retry takes it.
		answers.Text = notes
		_, err = s.Actors.RetryOn(ctx, item.ID, inbox.RetryInput{Answers: answers, Via: via, Findings: findings})
	case ActionMarkDone:
		if st.Implementation == nil || st.Implementation.HeadSHA == "" {
			return fmt.Errorf("%w: there is no reviewed commit to sign off", ErrStale)
		}
		_, err = s.Actors.MarkDone(ctx, item.ID, st.Implementation.HeadSHA, via)
	case ActionCheckCommits:
		_, err = s.Actors.CheckCommits(ctx, item.ID, via)
	case ActionSecondOpinion:
		_, err = s.Actors.RequestSecondOpinion(ctx, item.ID, inbox.SecondOpinionInput{Via: via})
	case ActionReject:
		if notes == "" {
			return fmt.Errorf("%w: say why the item is rejected", ErrInvalid)
		}
		_, err = s.Actors.Reject(ctx, item.ID, notes, via)
	}
	return err
}

// planToApprove is the plan revision an approval names: the one picked, or
// the gate's only plan. Its content hash comes from the state, never from
// the request.
func planToApprove(st *itemstate.State, picked string) (*itemstate.RevisionView, error) {
	revs := revisions(st)
	if picked == "" {
		if len(revs) == 1 {
			return &revs[0], nil
		}
		if len(revs) == 0 {
			return nil, fmt.Errorf("%w: there is no plan to approve", ErrInvalid)
		}
		return nil, fmt.Errorf("%w: pick the plan to approve", ErrInvalid)
	}
	for i := range revs {
		if revs[i].RevisionID == picked {
			return &revs[i], nil
		}
	}
	return nil, fmt.Errorf("%w: that plan is not at the gate any more", ErrStale)
}

// parseAction reads the action as a renderer gives it, or wrapped in its
// A2UI message.
func parseAction(raw json.RawMessage) (*action, error) {
	var wrapped struct {
		Action *action `json:"action"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Action != nil {
		return wrapped.Action, nil
	}
	var a action
	if err := json.Unmarshal(raw, &a); err != nil || a.Name == "" {
		return nil, fmt.Errorf("%w: action is an A2UI action with a name and a context", ErrInvalid)
	}
	return &a, nil
}

// firstOption is a picked option's letter: a choice picker binds a list of
// values, a host may send the one value as a string.
func firstOption(raw json.RawMessage) string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		if len(list) == 0 {
			return ""
		}
		return strings.TrimSpace(list[0])
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return strings.TrimSpace(one)
	}
	return ""
}

// forPerson is a refusal as a host shows it to the person who pressed the
// button: the reason, without the error's kind in front of it.
func forPerson(err error) string {
	msg := err.Error()
	for _, kind := range []error{ErrInvalid, ErrStale, inbox.ErrInvalidInput, auth.ErrApproverRequired,
		inbox.ErrSecondOpinionConflict, inbox.ErrPolicyRefused} {
		msg = strings.TrimPrefix(msg, kind.Error()+": ")
	}
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// pick is the item a card shows: the newest not closed, else the newest
// (ids are time-ordered).
func pick(items []inbox.Item) *inbox.Item {
	if len(items) == 0 {
		return nil
	}
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b inbox.Item) int { return strings.Compare(b.ID, a.ID) })
	for i := range sorted {
		switch sorted[i].Status {
		case inbox.StatusDone, inbox.StatusDiscarded, inbox.StatusConverted:
			continue
		}
		return &sorted[i]
	}
	return &sorted[0]
}

// Handler serves POST /api/inbox/card.
type Handler struct{ service *Service }

func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// Post answers the card of a reference. The reference is the URL's ref
// (POST /api/inbox/card?ref=JOB100235: a host fills it into the address it
// is set up with), or the body's subject.ref; given in both, they must agree.
// The body is optional with a ref in the URL.
func (h *Handler) Post(w http.ResponseWriter, r *http.Request) {
	var req Request
	if r.ContentLength != 0 {
		// A body of unknown length (chunked) may still be empty: EOF before
		// any JSON is no body.
		if err := rest.DecodeJSONStrict(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
			rest.RespondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	if ref := strings.TrimSpace(r.URL.Query().Get("ref")); ref != "" {
		if body := strings.TrimSpace(req.Subject.Ref); body != "" && body != ref {
			rest.RespondError(w, http.StatusBadRequest, "The reference in the address ("+ref+") and in the body ("+body+") differ.")
			return
		}
		req.Subject.Ref = ref
	}
	env, err := h.service.Render(r.Context(), req, r.URL.Query().Get("workspace"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalid), errors.Is(err, inbox.ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, forPerson(err))
		case errors.Is(err, ErrStale):
			reason := strings.TrimPrefix(err.Error(), ErrStale.Error()+": ")
			rest.RespondError(w, http.StatusConflict, "This card is out of date: "+reason+". Look at it again before you act.")
		case errors.Is(err, inbox.ErrStaleRevision), errors.Is(err, inbox.ErrSecondOpinionConflict), errors.Is(err, inbox.ErrAlreadyConverted):
			// The inbox refused the action: its own refusals, as its
			// endpoints answer them.
			rest.RespondError(w, http.StatusConflict, forPerson(err))
		case errors.Is(err, auth.ErrApproverRequired), errors.Is(err, inbox.ErrPolicyRefused):
			rest.RespondError(w, http.StatusForbidden, forPerson(err))
		case errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, inbox.ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, "item not found")
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, env)
}
