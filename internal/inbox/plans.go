package inbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// The plans of an item as one document: every planner's plan of a planning
// round in full, the judge's comparison and the synthesis — for reading
// them side by side, and for taking them out as Markdown. It is cut from
// the item's own sections (what each planner wrote is appended there), so
// it reads the same as the item does.

// PlanDoc is one plan as its planner wrote it.
type PlanDoc struct {
	// Kind: "plan" (the one plan of a single planner), "candidate" (one of
	// several) or "synthesis" (the candidate merged from the others).
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	// Title: "Plan", or "Candidate 2 · codex" — numbered as the gate, Slack
	// and the comparison number them.
	Title      string `json:"title"`
	RevisionID string `json:"revision_id,omitempty"`
	Markdown   string `json:"markdown"`
	HTML       string `json:"html"`
	// The questions this planner asked with the plan.
	QuestionsMarkdown string `json:"questions_markdown,omitempty"`
	QuestionsHTML     string `json:"questions_html,omitempty"`
}

// PlanNote is a section beside the plans: the comparison, the round's
// summary, the approver's notes that closed the round.
type PlanNote struct {
	Heading  string `json:"heading"`
	Markdown string `json:"markdown"`
	HTML     string `json:"html"`
}

// PlansView is one planning round of an item.
type PlansView struct {
	ItemID string `json:"item_id"`
	Title  string `json:"title"`
	// Round is the planning round shown (1-based), Rounds how many the
	// item has: a re-plan after "request changes" starts a new one.
	Round      int        `json:"round"`
	Rounds     int        `json:"rounds"`
	Plans      []PlanDoc  `json:"plans"`
	Comparison *PlanNote  `json:"comparison,omitempty"`
	Summary    *PlanNote  `json:"summary,omitempty"`
	Notes      []PlanNote `json:"notes,omitempty"`
}

var reRevisionRef = regexp.MustCompile(`\*\*Revision:\*\*\s+([0-9a-fA-F-]{36})`)

// planRounds is the item's planning rounds, oldest first.
func planRounds(content string) (brief string, rounds []itemtext.Round) {
	brief, secs := itemtext.Split(content)
	for _, r := range itemtext.Rounds(secs) {
		if r.Kind == "plan" {
			rounds = append(rounds, r)
		}
	}
	return brief, rounds
}

// pickRound returns round n (1-based) of the rounds, the newest for 0.
func pickRound(rounds []itemtext.Round, n int) (itemtext.Round, int, error) {
	if len(rounds) == 0 {
		return itemtext.Round{}, 0, fmt.Errorf("%w: the item has no plan yet", ErrNotFound)
	}
	if n == 0 {
		n = len(rounds)
	}
	if n < 1 || n > len(rounds) {
		return itemtext.Round{}, 0, fmt.Errorf("%w: the item has %d planning round(s), not a round %d", ErrInvalidInput, len(rounds), n)
	}
	return rounds[n-1], n, nil
}

// PlansOf cuts planning round n (the newest for 0) out of an item's content.
func PlansOf(itemID, title, content string, n int) (*PlansView, error) {
	_, rounds := planRounds(content)
	round, n, err := pickRound(rounds, n)
	if err != nil {
		return nil, err
	}
	v := &PlansView{ItemID: itemID, Title: title, Round: n, Rounds: len(rounds), Plans: []PlanDoc{}}
	note := func(s itemtext.Section) *PlanNote {
		return &PlanNote{Heading: s.Heading, Markdown: s.Body, HTML: RenderMarkdown(s.Body)}
	}
	candidates := 0
	for _, s := range round.Sections {
		switch {
		case itemtext.HeadingIs(s, "Candidate — "), itemtext.HeadingIs(s, "Enrichment — skopos worker"):
			d := PlanDoc{Kind: "plan", Title: "Plan", Markdown: s.Body, HTML: RenderMarkdown(s.Body)}
			if itemtext.HeadingIs(s, "Candidate — ") {
				candidates++
				d.Label = strings.TrimSpace(strings.TrimPrefix(s.Heading, "Candidate — "))
				d.Kind = "candidate"
				if d.Label == "synthesis" {
					d.Kind = "synthesis"
				}
				d.Title = "Candidate " + strconv.Itoa(candidates) + " · " + d.Label
			}
			if m := reRevisionRef.FindStringSubmatch(s.Body); m != nil {
				d.RevisionID = m[1]
			}
			v.Plans = append(v.Plans, d)
		case itemtext.HeadingIs(s, "Questions — planner"):
			// The planner's questions belong to its plan: the candidate of
			// the same label, else the plan written just before them.
			label := itemtext.QuestionsLabel(s)
			at := len(v.Plans) - 1
			for i, d := range v.Plans {
				if label != "" && d.Label == label {
					at = i
				}
			}
			if at >= 0 {
				v.Plans[at].QuestionsMarkdown, v.Plans[at].QuestionsHTML = s.Body, RenderMarkdown(s.Body)
			}
		case itemtext.HeadingIs(s, "Comparison — "):
			v.Comparison = note(s)
		case itemtext.HeadingIs(s, "Plan candidates — round"):
			v.Summary = note(s)
		case itemtext.IsHumanNotes(s):
			v.Notes = append(v.Notes, *note(s))
		}
	}
	// Several plain plans in one round (a planner that ran again before
	// anyone answered) are told apart by their order.
	plain := 0
	for i := range v.Plans {
		if v.Plans[i].Kind == "plan" {
			plain++
			if plain > 1 {
				v.Plans[i].Title = "Plan (" + strconv.Itoa(plain) + ")"
			}
		}
	}
	return v, nil
}

// PlansMarkdown is planning round n (the newest for 0) as one Markdown
// document: the item's title and brief, then every section of the round as
// it stands in the item — each plan, its questions, the comparison, the
// synthesis and the approver's notes.
func PlansMarkdown(itemID, title, content string, n int) (string, error) {
	brief, rounds := planRounds(content)
	round, n, err := pickRound(rounds, n)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", strings.TrimSpace(title))
	fmt.Fprintf(&b, "_Item %s · planning round %d of %d_\n\n", itemID, n, len(rounds))
	if brief = strings.TrimSpace(brief); brief != "" {
		b.WriteString(brief + "\n\n")
	}
	for _, s := range round.Sections {
		b.WriteString(strings.TrimSpace(s.Text()) + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// Plans is planning round n (the newest for 0) of an item the caller may read.
func (s *Service) Plans(ctx context.Context, itemID string, n int) (*PlansView, *Item, error) {
	item, err := s.GetItem(ctx, itemID)
	if err != nil {
		return nil, nil, err
	}
	v, err := PlansOf(item.ID, item.Title, item.Content, n)
	return v, item, err
}

var reSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// plansFileName names the exported file after the item: its title in
// lowercase words, or its id when the title has none.
func plansFileName(item *Item) string {
	slug := strings.Trim(reSlugRun.ReplaceAllString(strings.ToLower(item.Title), "-"), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = item.ID
	}
	return slug + "-plans.md"
}

// ItemPlans handles GET /api/inbox/{id}/plans[?round=N][&format=md]: the
// plans of a planning round (the newest by default) — every planner's plan
// in full with its questions, the comparison and the synthesis. format=md
// answers with one Markdown document to save.
func (h *Handler) ItemPlans(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	round := 0
	if q := strings.TrimSpace(r.URL.Query().Get("round")); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			rest.RespondError(w, http.StatusBadRequest, "round is a number from 1")
			return
		}
		round = n
	}
	v, item, err := h.service.Plans(r.Context(), r.PathValue("id"), round)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	switch format := r.URL.Query().Get("format"); format {
	case "":
		rest.RespondJSON(w, http.StatusOK, v)
	case "md", "markdown":
		doc, err := PlansMarkdown(item.ID, item.Title, item.Content, round)
		if err != nil {
			rest.InternalError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+plansFileName(item)+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(doc))
	default:
		rest.RespondError(w, http.StatusBadRequest, "format is md, or left out for JSON")
	}
}
