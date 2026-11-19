package blackboard

import (
	"fmt"
	"slices"
	"strings"
)

// MaxAgentBytes bounds the blackboard text one MCP read hands an agent
// (about 8,000 tokens). A workspace's project-wide entries, bugs and debt
// come with every read whatever the branch, and they grow without limit: a
// read that returned all of them filled an agent's context (300 KB once) or
// did not fit at all.
const MaxAgentBytes = 32 << 10

// ForAgent renders the bundle for an agent, once (the MCP answer carries no
// second copy as JSON), within max bytes: the branch's and session's own
// entries first, then the rest newest first; each entry with its id (what
// blackboard_delete takes). shown is how many entries the text holds; when
// it is fewer than the bundle's, the text ends saying how many were left
// out and how to narrow the read.
func (b *Bundle) ForAgent(branch string, max int) (text string, shown int) {
	if b == nil {
		return formatMarkdown(branch, nil), 0
	}
	order := slices.Clone(b.Entries)
	// The read's own: its session's entries and the asked branch's. Bugs
	// and debt of other branches come with every read and are not.
	own := func(e Entry) bool {
		return e.Scope == ScopeSession || (e.Scope == ScopeBranch && branch != "" && e.BranchName == branch)
	}
	slices.SortStableFunc(order, func(a, c Entry) int {
		if own(a) != own(c) {
			if own(a) {
				return -1
			}
			return 1
		}
		return c.UpdatedAt.Compare(a.UpdatedAt) // newest first
	})
	// The header, a heading per entry type and the closing line.
	size := len(formatMarkdown(branch, nil)) + 6*48 + 256
	var keep []Entry
	for _, e := range order {
		var sb strings.Builder
		writeEntry(&sb, e)
		if size+sb.Len() > max && len(keep) > 0 {
			break // what is left out is always the oldest
		}
		size += sb.Len()
		keep = append(keep, e)
	}
	text = formatMarkdown(branch, keep)
	if left := len(b.Entries) - len(keep); left > 0 {
		text += fmt.Sprintf("_%d more entries are not shown here (%d in all; the branch's own and the newest come first). "+
			"Narrow the read with `q` (text), `entry_type` or `author`._\n", left, len(b.Entries))
	}
	return text, len(keep)
}

// writeEntry renders one entry as formatMarkdown lists it.
func writeEntry(sb *strings.Builder, e Entry) {
	ref := ""
	if e.CodeRef != "" {
		ref = fmt.Sprintf(" (%s)", sanitizeEntryText(e.CodeRef))
	}
	fmt.Fprintf(sb, "- **%s**%s\n", sanitizeEntryText(e.Title), ref)
	if e.Content != "" {
		fmt.Fprintf(sb, "  %s\n", sanitizeEntryText(e.Content))
	}
	fmt.Fprintf(sb, "  _— %s, id %s_\n", sanitizeEntryText(e.AuthorAgentID), e.ID)
}
