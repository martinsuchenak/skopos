package parse

func init() {
	base := *commonProfile
	base.name = "java"
	base.relationNodes = javaRelations()
	registerProfile(&base)
}
