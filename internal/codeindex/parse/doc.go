package parse

import (
	"strings"
	"unicode/utf8"

	gts "github.com/odvcencio/gotreesitter"
)

// Truncate cuts s to at most maxBytes without splitting a UTF-8 rune.
func Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// maxDocBytes caps the stored documentation per symbol. Docs are paid for
// three times (blob size, FTS index, embedding text); beyond this there is
// no retrieval value.
const maxDocBytes = 1024

// docForNode extracts the documentation attached to a definition node.
// Declaration data always wins: doc tags that restate the signature
// (@param, @return, @var, …) are dropped — they frequently lag the real
// code — and only what the declaration cannot express is kept: the summary
// and tags like @throws, @deprecated, @see.
//
// Attachment rules: the comment block immediately preceding the declaration
// (contiguous line-comment runs count as one block; annotations/attributes
// between the doc and the declaration are skipped). Profiles can supply
// docFromBody for languages where the doc is part of the declaration
// itself (Python docstrings).
func docForNode(prof *langProfile, n *gts.Node, lang *gts.Language, src []byte) string {
	if prof != nil && prof.docFromBody != nil {
		if d := prof.docFromBody(n, lang, src); d != "" {
			return cleanDoc(d)
		}
	}
	if d := docFromSiblingsAbove(n, lang, src); d != "" {
		return d
	}
	// Modifier wrappers (JS `export function …`) own the comment's
	// adjacency: the definition is a child of the export statement.
	if p := n.Parent(); p != nil {
		switch p.Type(lang) {
		case "export_statement", "export_declaration":
			return docFromSiblingsAbove(p, lang, src)
		}
	}
	return ""
}

// docFromSiblingsAbove collects the comment block directly above a node,
// skipping attribute/annotation siblings that may sit between them.
func docFromSiblingsAbove(n *gts.Node, lang *gts.Language, src []byte) string {
	cur := n.PrevSibling()
	for cur != nil && isDocBarrier(cur.Type(lang)) {
		cur = cur.PrevSibling()
	}
	if cur == nil || !isCommentNode(cur.Type(lang)) {
		return ""
	}
	endRow := int(n.StartPoint().Row)
	var parts []string // collected comment texts, source order
	for c := cur; c != nil && isCommentNode(c.Type(lang)); c = c.PrevSibling() {
		cStart, cEnd := int(c.StartPoint().Row), int(c.EndPoint().Row)
		// The block must sit directly above the declaration (or the comment
		// above it) — no blank-line gaps.
		if cEnd != endRow-1 {
			break
		}
		parts = append([]string{string(src[c.StartByte():c.EndByte()])}, parts...)
		if isBlockComment(c.Type(lang), src, c) {
			break // /** … */ stands alone
		}
		endRow = cStart
	}
	return cleanDoc(strings.Join(parts, "\n"))
}

func isCommentNode(t string) bool {
	switch t {
	case "comment", "line_comment", "block_comment", "documentation":
		return true
	}
	return false
}

// isDocBarrier reports a sibling type that may legitimately sit between a
// doc comment and the declaration it documents.
func isDocBarrier(t string) bool {
	t = strings.ToLower(t)
	return strings.Contains(t, "attribute") || strings.Contains(t, "annotation") || strings.Contains(t, "marker")
}

// isBlockComment distinguishes /* … */ (and triple-quoted) comments from
// line comments; block comments are never merged with neighbours.
func isBlockComment(t string, src []byte, n *gts.Node) bool {
	if t == "block_comment" {
		return true
	}
	b := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
	return strings.HasPrefix(b, "/*")
}

// cleanDoc strips comment syntax, applies the keep/drop tag policy, and
// caps the result. Returns "" for blocks with nothing worth keeping.
func cleanDoc(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(raw, "\n") {
		l = stripCommentLine(l)
		if l == "" {
			if len(lines) > 0 && lines[len(lines)-1] != "" {
				lines = append(lines, "") // keep paragraph breaks
			}
			continue
		}
		lines = append(lines, l)
	}
	// Drop a trailing blank introduced above.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var (
		out      strings.Builder
		tagBlock bool
	)
	for _, l := range lines {
		lt := strings.TrimSpace(l)
		if strings.HasPrefix(lt, "@") {
			tag := lt
			if i := strings.IndexAny(tag, " \t"); i > 0 {
				tag = tag[:i]
			}
			if !keepDocTag(tag) {
				continue // signature restatements: the declaration wins
			}
			if out.Len() > 0 && !tagBlock {
				out.WriteString("\n\n")
			}
			tagBlock = true
			out.WriteString(lt)
			out.WriteString("\n")
			continue
		}
		if tagBlock {
			// Description text after the tags (rare layout) — keep flowing.
			out.WriteString(lt)
			out.WriteString("\n")
			continue
		}
		out.WriteString(l)
		out.WriteString("\n")
	}
	doc := strings.TrimRight(out.String(), "\n")
	doc = Truncate(doc, maxDocBytes)
	return strings.TrimSpace(doc)
}

// keepDocTag: tags kept are those expressing what the declaration cannot.
// @param/@return/@var/@type restate the signature and are routinely stale.
func keepDocTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "@throws", "@throw", "@deprecated", "@see", "@since", "@internal":
		return true
	}
	return false
}

// stripCommentLine removes per-line comment syntax.
func stripCommentLine(l string) string {
	l = strings.TrimLeft(l, " \t")
	switch {
	case strings.HasPrefix(l, "/**"), strings.HasPrefix(l, "/*"):
		l = l[2:]
	case strings.HasPrefix(l, "//"):
		l = l[2:]
	case strings.HasPrefix(l, "#"):
		l = l[1:]
	case strings.HasPrefix(l, "--"):
		l = l[2:]
	}
	l = strings.TrimRight(l, " \t")
	// Block-comment closers and the leading " * " continuation.
	l = strings.TrimSuffix(l, "*/")
	l = strings.TrimLeft(l, " \t")
	l = strings.TrimPrefix(l, "*")
	return strings.TrimRight(l, " \t")
}
