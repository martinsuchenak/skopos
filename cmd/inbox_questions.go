package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/paularlott/cli"
)

// `skopos inbox questions` reads an item's question-and-answer record;
// `skopos inbox answer` answers (or corrects) without moving the item.

func inboxQuestionCmds() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "questions",
			Usage: "Show an item's questions and answers: who asked, the state of each question, who answered and how",
			Flags: workflowFlags(),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				var out struct {
					Sets []qa.Set    `json:"sets"`
					Free []qa.Answer `json:"free"`
					Open int         `json:"open"`
				}
				if err := c.call(ctx, http.MethodGet, "/api/inbox/"+url.PathEscape(id)+"/questions", nil, &out, "reading the questions"); err != nil {
					return err
				}
				printQuestions(os.Stdout, out.Sets, out.Open)
				if len(out.Free) > 0 {
					fmt.Println("\nAnswers given when no question was open:")
					for _, a := range out.Free {
						fmt.Printf("  %s\n", answerLine(a))
					}
				}
				return nil
			},
		},
		{
			Name:  "answer",
			Usage: "Answer the open questions without approving or sending the item back (while it waits for you)",
			Flags: workflowFlags(
				&cli.StringSliceFlag{Name: "answer", Usage: `One answer, "<n>=<option letter or words>" (repeatable): --answer 1=a --answer "2=Redis, for now"`},
				&cli.StringFlag{Name: "text", Usage: "Free text, read against the open questions (lines like \"1) a\"; the rest answers the round as a whole)"},
				&cli.StringFlag{Name: "replaces", Usage: "With --question: correct an answer, naming the answer it replaces"},
				&cli.StringFlag{Name: "question", Usage: "With --replaces: the question id the correction is for"},
				&cli.StringFlag{Name: "option", Usage: "With --replaces: the corrected option letter"},
				&cli.StringFlag{Name: "words", Usage: "With --replaces: the corrected answer in words"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				var answers any
				if r := strings.TrimSpace(cmd.GetString("replaces")); r != "" {
					q := strings.TrimSpace(cmd.GetString("question"))
					if q == "" {
						return fmt.Errorf("--replaces needs --question (the question id) and --option or --words")
					}
					answers = []qa.AnswerItem{{QuestionID: q, Option: cmd.GetString("option"), Text: cmd.GetString("words"), Replaces: r}}
				} else {
					text, err := answerText(cmd.GetStringSlice("answer"), cmd.GetString("text"))
					if err != nil {
						return err
					}
					answers = text
				}
				var item struct {
					Status         string     `json:"status"`
					AnswersMatched []qa.Match `json:"answers_matched"`
				}
				if err := c.call(ctx, http.MethodPost, "/api/inbox/"+url.PathEscape(id)+"/answers", map[string]any{"answers": answers}, &item, "answering"); err != nil {
					return err
				}
				printMatched(os.Stdout, item.AnswersMatched)
				return nil
			},
		},
	}
}

// answerText turns --answer n=value flags (and --text) into free text the
// server reads: "1) a", "2) Redis, for now".
func answerText(flags []string, text string) (string, error) {
	var lines []string
	for _, f := range flags {
		n, v, ok := strings.Cut(f, "=")
		n, v = strings.TrimSpace(n), strings.TrimSpace(v)
		if !ok || n == "" || v == "" {
			return "", fmt.Errorf("--answer %q: use <n>=<option letter or words>", f)
		}
		lines = append(lines, n+") "+v)
	}
	if t := strings.TrimSpace(text); t != "" {
		lines = append(lines, t)
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("give --answer <n>=<value> (repeatable) or --text")
	}
	return strings.Join(lines, "\n"), nil
}

func printMatched(w io.Writer, ms []qa.Match) {
	if len(ms) == 0 {
		_, _ = fmt.Fprintln(w, "recorded as written: no open question matched")
		return
	}
	for _, m := range ms {
		switch {
		case m.QuestionID != "" && m.Replaces != "":
			_, _ = fmt.Fprintf(w, "corrected question %d (%s)\n", m.N, m.QuestionID)
		case m.QuestionID != "":
			opt := ""
			if m.Option != "" {
				opt = " (" + m.Option + ")"
			}
			_, _ = fmt.Fprintf(w, "answered question %d%s\n", m.N, opt)
		case m.General:
			_, _ = fmt.Fprintf(w, "answer to the round's questions: %s\n", m.Line)
		}
	}
}

func printQuestions(w io.Writer, sets []qa.Set, open int) {
	if len(sets) == 0 {
		_, _ = fmt.Fprintln(w, "no questions")
		return
	}
	for _, s := range sets {
		who := s.Asked()
		if who == "" {
			who = "the " + s.Asker
		}
		about := ""
		if s.SubjectKind != qa.SubjectNone && s.Subject != "" {
			about = ", about " + strings.ReplaceAll(s.SubjectKind, "_", " ") + " " + s.Subject
		}
		run := ""
		if s.RunID != "" {
			run = ", run " + s.RunID
		}
		_, _ = fmt.Fprintf(w, "\nAsked by %s (%s%s%s), %s\n", who, s.CreatedAt.Format("2006-01-02 15:04"), run, about, s.ID)
		for _, q := range s.Questions {
			_, _ = fmt.Fprintf(w, "  %d. %s  [%s]  %s\n", q.N, q.Text, strings.ReplaceAll(q.State, "_", " "), q.ID)
			for i, o := range q.Options {
				_, _ = fmt.Fprintf(w, "     (%s) %s\n", qa.OptionLetterOf(i), o)
			}
			for _, a := range q.Answers {
				_, _ = fmt.Fprintf(w, "     %s\n", answerLine(a))
			}
		}
		for _, a := range s.General {
			_, _ = fmt.Fprintf(w, "  %s\n", answerLine(a))
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d open\n", open)
}

func answerLine(a qa.Answer) string {
	what := a.Text
	if a.Option != "" {
		what = strings.TrimSpace("(" + a.Option + ") " + a.Text)
	}
	kind := "answer"
	switch {
	case a.Kind == qa.KindSkipped:
		kind, what = "skipped", "left unanswered"
	case a.Kind == qa.KindGeneral:
		kind = "free text"
	case a.Replaces != "":
		kind = "correction"
	}
	extra := ""
	if a.Replaced {
		extra += ", replaced"
	}
	if a.AfterUse {
		extra += ", after a run used the earlier answer"
	}
	if a.UsedByRun != "" {
		extra += ", used by run " + a.UsedByRun
	}
	return fmt.Sprintf("%s: %s — %s via %s, %s, %s%s  %s", kind, what, a.Actor, a.Via, a.Action, a.CreatedAt.Format("2006-01-02 15:04"), extra, a.ID)
}
