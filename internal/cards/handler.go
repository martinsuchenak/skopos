package cards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemstate"
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

// Service builds cards.
type Service struct {
	Items  Items
	States States
	// PublicURL is the server's public address, for the card's link.
	PublicURL string
}

// Request is what a host sends.
type Request struct {
	Subject Subject `json:"subject"`
	// Action is an A2UI action message from a card's button. Cards carry
	// no buttons yet, so a request with one is refused.
	Action json.RawMessage `json:"action,omitempty"`
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
		return nil, fmt.Errorf("%w: subject.ref is required, at most %d bytes", ErrInvalid, maxRef)
	}
	if len(req.Subject.Text) > maxSubjectText {
		return nil, fmt.Errorf("%w: subject.text is at most %d bytes", ErrInvalid, maxSubjectText)
	}
	if len(req.Action) > 0 && string(req.Action) != "null" {
		return nil, fmt.Errorf("%w: this server shows cards but takes no actions on them yet", ErrInvalid)
	}
	req.Subject.Ref = ref
	items, err := s.Items.ListItemsFilter(ctx, inbox.ListFilter{WorkspaceID: strings.TrimSpace(workspace), Ref: ref})
	if err != nil {
		return nil, err
	}
	item := pick(items)
	if item == nil {
		return Build(Input{Subject: req.Subject, PublicURL: s.PublicURL}), nil
	}
	st, err := s.States.Get(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	in := Input{Subject: req.Subject, State: st, Outcome: item.Outcome, PublicURL: s.PublicURL}
	// Whether the newest plan round found nothing to do matters at the plan
	// gate and on a closed item; it is read from the item's own text, by
	// the rule the approval checks.
	switch inbox.Status(st.Item.Status) {
	case inbox.StatusAwaitingApproval, inbox.StatusDone:
		full, err := s.Items.ReadItem(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		in.NothingToDo = inbox.NewestRoundNothingToDo(full.Content)
	}
	return Build(in), nil
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

func (h *Handler) Post(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := rest.DecodeJSONStrict(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	env, err := h.service.Render(r.Context(), req, r.URL.Query().Get("workspace"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalid), errors.Is(err, inbox.ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
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
