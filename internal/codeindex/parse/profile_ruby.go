package parse

func init() {
	base := *commonProfile
	base.name = "ruby"
	base.relationNodes = rubyRelations()
	registerProfile(&base)
}
