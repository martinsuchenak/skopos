package parse

import gts "github.com/odvcencio/gotreesitter"

func init() {
	base := *commonProfile
	base.name = "rust"
	base.defName = func(n *gts.Node, lang *gts.Language, src []byte) (string, bool) {
		if n.Type(lang) == "impl_item" {
			return rustImplName(n, lang, src)
		}
		return "", false
	}
	base.relationNodes = rustRelations()
	registerProfile(&base)
}
