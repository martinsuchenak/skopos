// CSS specifics: selectors are the language's definitions — class selectors
// (.card), id selectors (#main), and tag selectors (button) become symbols
// so stylesheets are searchable alongside code. Nested/descendant selectors
// each extract individually (the walk visits every selector node).
package parse

func init() {
	base := *commonProfile
	base.name = "css"
	base.defs = map[string]string{
		"class_selector": "class",
		"id_selector":    "id",
		"type_selector":  "tag",
		"tag_name":       "tag", // bare tag in some grammar shapes
	}
	base.calls = map[string]bool{} // stylesheets have no call edges
	base.containerKinds = map[string]bool{}
	base.selfReceivers = map[string]bool{}
	base.paramListNodes = nil
	base.assignmentNodes = map[string]bool{}
	registerProfile(&base)
}
