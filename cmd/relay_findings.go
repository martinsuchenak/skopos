package cmd

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/inbox"
)

// Review findings in the Slack thread: the gate messages list the ones
// something is still owed on, numbered; `changes 2,3: <notes>` and
// `retry 2: <answer>` send findings back by those numbers, `waive 2: <why>`
// accepts one, `findings` lists them again.

// A findings list has a budget of its own: the notifier cuts a message at
// 2800 bytes, and a list that ran into that would lose the commands under
// it while the relay still knew numbers nobody was shown.
const (
	maxShownFindings  = 12
	maxFindingsBytes  = 1100
	maxFindingTextLen = 160
)

// reFindingNumbers is a selector that names findings: "2" or "2,3".
var reFindingNumbers = regexp.MustCompile(`^\d+(,\d+)*$`)

// slackText makes agent-written text safe to put in a message: Slack reads
// <…> as a link, a mention or a channel-wide ping, and & starts an entity.
func slackText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// findingStateWord is a finding's state as a message says it.
func findingStateWord(f findings.Finding) string {
	switch f.State {
	case findings.StateOpen:
		return "to fix"
	case findings.StateStillOpen:
		return "still open"
	case findings.StateFixed:
		return "fixed, not confirmed"
	case findings.StateNoted:
		return "not followed up"
	}
	return strings.ReplaceAll(f.State, "_", " ")
}

// findingsBlock is the item's findings something is still owed on — or
// that nobody follows up — as a numbered list, with the rest counted. It
// remembers exactly the numbering it shows, for the replies that use it.
// "" when the item has no findings.
func (r *slackRelay) findingsBlock(itemID string) string {
	all, err := r.inbox.Findings(relayCtx(), itemID)
	if err != nil || len(all) == 0 {
		r.forgetFindings(itemID)
		return ""
	}
	// What needs a decision first: still open, to fix, claimed fixed, then
	// what nobody follows; blocking before nits within each.
	order := map[string]int{findings.StateStillOpen: 0, findings.StateOpen: 1, findings.StateFixed: 2, findings.StateNoted: 3}
	var listed []findings.Finding
	for rank := 0; rank <= 3; rank++ {
		for _, sev := range []string{findings.SeverityBlocking, findings.SeverityNit} {
			for _, f := range all {
				if o, ok := order[f.State]; ok && o == rank && f.Severity == sev {
					listed = append(listed, f)
				}
			}
		}
	}
	addressed, waived := 0, 0
	for _, f := range all {
		switch f.State {
		case findings.StateAddressed:
			addressed++
		case findings.StateWaived:
			waived++
		}
	}
	ids := make([]string, 0, len(listed))
	var b strings.Builder
	for i, f := range listed {
		line := fmt.Sprintf("%d. _%s · %s_ %s _(%s)_\n", i+1, findingStateWord(f), f.Severity, truncate(slackText(f.Text), maxFindingTextLen), slackText(f.By()))
		if i >= maxShownFindings || b.Len()+len(line) > maxFindingsBytes {
			break
		}
		b.WriteString(line)
		ids = append(ids, f.ID)
	}
	r.mu.Lock()
	if r.shownFindings == nil {
		r.shownFindings = map[string][]string{}
	}
	r.shownFindings[itemID] = ids
	r.mu.Unlock()
	var tail []string
	if more := len(listed) - len(ids); more > 0 {
		tail = append(tail, fmt.Sprintf("%d more (see the dashboard)", more))
	}
	if addressed > 0 {
		tail = append(tail, fmt.Sprintf("%d addressed", addressed))
	}
	if waived > 0 {
		tail = append(tail, fmt.Sprintf("%d waived", waived))
	}
	out := "*Findings*\n" + b.String()
	if len(tail) > 0 {
		out += "_" + strings.Join(tail, ", ") + "_\n"
	}
	return out
}

// forgetFindings drops the numbering remembered for an item: a message
// that shows no list must not leave an older list's numbers in force.
func (r *slackRelay) forgetFindings(itemID string) {
	r.mu.Lock()
	delete(r.shownFindings, itemID)
	r.mu.Unlock()
}

// pickFindings resolves "2,3" against the numbering the last message
// showed. msg says why not, when it cannot.
func (r *slackRelay) pickFindings(itemID, sel string) (ids []string, msg string) {
	r.mu.Lock()
	shown, known := r.shownFindings[itemID]
	r.mu.Unlock()
	if !known {
		// After a restart, or a message without a list, the relay does not
		// know what was read.
		return nil, "I don't know which list you mean — reply `findings` for the current one, then use its numbers."
	}
	if len(shown) == 0 {
		return nil, "There is no finding to pick: all of them are addressed or waived."
	}
	for _, part := range strings.Split(sel, ",") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(shown) {
			return nil, fmt.Sprintf("No finding %s — the list has %d.", part, len(shown))
		}
		ids = append(ids, shown[n-1])
	}
	return ids, ""
}

// isFindingCommand: commands about findings that are no workflow action —
// they apply at once, whatever the item is doing.
func isFindingCommand(cmd string) bool { return cmd == "waive" || cmd == "findings" }

// applyFindingCommand handles `findings` and `waive <n>[: <why>]`.
func (r *slackRelay) applyFindingCommand(ctx context.Context, item *inbox.Item, cmd, sel, arg string) bool {
	thread := item.SlackThreadTS
	switch cmd {
	case "findings":
		block := r.findingsBlock(item.ID)
		if block == "" {
			r.post(ctx, thread, "No review findings on this item.")
			return true
		}
		r.post(ctx, thread, block+findingHints(r.inbox.SendBackWith(relayCtx(), item)))
		return true
	case "waive":
		if !reFindingNumbers.MatchString(sel) || strings.Contains(sel, ",") {
			r.post(ctx, thread, "⚠️ Name one finding by its number: `waive 2: <why>` (`findings` lists them).")
			return true
		}
		ids, msg := r.pickFindings(item.ID, sel)
		if msg != "" {
			r.post(ctx, thread, "⚠️ "+msg)
			return true
		}
		f, err := r.inbox.WaiveFinding(relayCtx(), item.ID, ids[0], arg, audit.ViaSlack)
		if err != nil {
			r.post(ctx, thread, "⚠️ "+truncate(err.Error(), 300))
			return true
		}
		r.post(ctx, thread, "🤝 Waived: "+truncate(slackText(f.Text), 200))
		return true
	}
	return false
}

// findingHints is the commands a findings list can be answered with, by the
// action that sends findings back now (inbox.SendBackWith; "" = none does,
// so only waive is offered).
func findingHints(sendBack string) string {
	switch sendBack {
	case "request-changes":
		return "`changes 2,3: <notes>` sends findings back to be fixed · `waive 2: <why>` accepts one"
	case "retry":
		return "`retry 2,3: <answer>` has findings fixed too · `waive 2: <why>` accepts one"
	}
	return "`waive 2: <why>` accepts one"
}
