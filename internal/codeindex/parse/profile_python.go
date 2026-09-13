package parse

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
)

// Python: docstrings live inside the declaration (body > first statement >
// string), so there is no preceding-comment heuristic to apply.
func init() {
	base := *commonProfile
	base.name = "python"
	base.docFromBody = pythonDocString
	base.relationNodes = pythonRelations()
	base.importNodes = map[string]bool{"import_statement": true, "import_from_statement": true}
	registerProfile(&base)
}

func pythonDocString(n *gts.Node, lang *gts.Language, src []byte) string {
	body := childOfType(n, lang, "block")
	if body == nil {
		return ""
	}
	// The docstring is the block's first statement; some grammar builds
	// wrap it in an expression_statement, some expose the string directly.
	s := childOfType(body, lang, "string")
	if s == nil {
		if stmt := childOfType(body, lang, "expression_statement"); stmt != nil {
			s = childOfType(stmt, lang, "string")
		}
	}
	if s == nil {
		return ""
	}
	text := string(src[s.StartByte():s.EndByte()])
	// Triple-quoted first, then plain.
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(text, q) && strings.HasSuffix(text, q) && len(text) >= 2*len(q) {
			return strings.TrimSuffix(strings.TrimPrefix(text, q), q)
		}
	}
	if len(text) >= 2 && (text[0] == '"' || text[0] == '\'') && text[len(text)-1] == text[0] {
		return text[1 : len(text)-1]
	}
	return ""
}

func childOfType(n *gts.Node, lang *gts.Language, typ string) *gts.Node {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.Type(lang) == typ {
			return c
		}
	}
	return nil
}
