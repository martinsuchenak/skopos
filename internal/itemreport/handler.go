package itemreport

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Handler serves GET /api/inbox/{id}/report.
type Handler struct{ service *Service }

func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// Get answers with the item's report: ?format=md (the default), html or
// json; ?level=full (the default) or summary for md and html. JSON is the
// whole structure at either level.
//
// The report holds the plan and every run's result, so a capped key needs
// plans:read and runs:read beside the item's own area: with one missing
// the answer is 403, never a report with a part left out. A run's own
// token is refused: the report holds the other reviewers' results.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if p := auth.PrincipalFromContext(r.Context()); p != nil {
		if p.Run != nil {
			rest.RespondError(w, http.StatusForbidden, "an item's report is not for a run")
			return
		}
		if !p.Allows(auth.AreaPlans, false) || !p.Allows(auth.AreaRuns, false) {
			rest.RespondError(w, http.StatusForbidden, "an item's report needs the plans:read and runs:read capabilities")
			return
		}
	}
	q := r.URL.Query()
	level := strings.ToLower(strings.TrimSpace(q.Get("level")))
	switch level {
	case "":
		level = LevelFull
	case LevelFull, LevelSummary:
	default:
		rest.RespondError(w, http.StatusBadRequest, "level is full or summary")
		return
	}
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	switch format {
	case "", "markdown":
		format = "md"
	case "md", "html", "json":
	default:
		rest.RespondError(w, http.StatusBadRequest, "format is md, html or json")
		return
	}

	rep, err := h.service.Build(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, inbox.ErrNotFound), errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, "item not found")
		case errors.Is(err, inbox.ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	if format == "json" {
		rest.RespondJSON(w, http.StatusOK, rep)
		return
	}
	body, contentType := Markdown(rep, level), "text/markdown; charset=utf-8"
	if format == "html" {
		// A fragment of plain elements, through the dashboard's renderer:
		// raw HTML in an agent's text is escaped, never passed through.
		body, contentType = inbox.RenderMarkdown(body), "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+FileName(rep, level, format)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

var reSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// FileName names the saved report after the item: its reference or title
// in lowercase words, else its id.
func FileName(r *Report, level, format string) string {
	name := r.Item.Ref
	if name == "" {
		name = r.Item.Title
	}
	slug := strings.Trim(reSlugRun.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = r.Item.ID
	}
	if level == LevelSummary {
		slug += "-summary"
	} else {
		slug += "-report"
	}
	return slug + "." + format
}
