package inbox

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// questionFilter reads the decision log's filters: workspace_id, item_id,
// state (a question state, or free), asker, answered_by, from and to (a
// UTC date or an RFC 3339 time with its offset; to is exclusive, a date
// means its whole day). The CLI sends local days as times.
func questionFilter(r *http.Request) (qa.Filter, error) {
	q := r.URL.Query()
	f := qa.Filter{
		WorkspaceID: strings.TrimSpace(q.Get("workspace_id")), ItemID: strings.TrimSpace(q.Get("item_id")),
		State: strings.TrimSpace(q.Get("state")), Asker: strings.TrimSpace(q.Get("asker")),
		AnsweredBy: strings.TrimSpace(q.Get("answered_by")),
	}
	switch f.State {
	case "", qa.StateOpen, qa.StateAnswered, qa.StateAnsweredFreeText, qa.StateSkipped, qa.StateSuperseded, "free":
	default:
		return f, fmt.Errorf("unknown state %q", f.State)
	}
	var err error
	if f.From, err = parseWhen(q.Get("from"), false); err != nil {
		return f, fmt.Errorf("from: %w", err)
	}
	if f.To, err = parseWhen(q.Get("to"), true); err != nil {
		return f, fmt.Errorf("to: %w", err)
	}
	return f, nil
}

// parseWhen reads a date (2026-11-09) or an RFC 3339 time; end: a date
// means the end of its day (the start of the next).
func parseWhen(v string, end bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if d, err := time.Parse("2006-01-02", v); err == nil {
		if end {
			return d.AddDate(0, 0, 1), nil
		}
		return d, nil
	}
	return time.Parse(time.RFC3339, v)
}

// SearchQuestions handles GET /api/inbox/questions: the decision log across
// items (rows, oldest first, paged by limit/offset; format=csv for all
// matching rows as CSV).
func (h *Handler) SearchQuestions(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	f, err := questionFilter(r)
	if err != nil {
		rest.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := h.service.SearchQuestions(r.Context(), f)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="questions.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write(qa.CSVHeader)
		for _, row := range rows {
			_ = cw.Write(row.CSV())
		}
		cw.Flush()
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	total := len(rows)
	offset = min(max(offset, 0), total)
	page := rows[offset:min(offset+limit, total)]
	rest.RespondJSON(w, http.StatusOK, map[string]any{"rows": page, "total": total, "limit": limit, "offset": offset})
}

// QuestionSummary handles GET /api/inbox/questions/summary: the counts over
// the same filters — in total, per workspace and per person.
func (h *Handler) QuestionSummary(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	f, err := questionFilter(r)
	if err != nil {
		rest.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := h.service.SearchQuestions(r.Context(), f)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, qa.Summarize(rows))
}
