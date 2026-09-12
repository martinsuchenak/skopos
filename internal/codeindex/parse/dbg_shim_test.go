package parse

import (
	"fmt"
	"testing"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

func TestDbgModifierShapes(t *testing.T) {
	cases := []struct{ file, src string }{
		{"t.php", "<?php\nclass A {\n  #[Route('/reset', name: 'reset')]\n  public static function handle($r): void {}\n  abstract protected function make();\n}\n"},
		{"t.ts", "class A {\n  private static async run(): Promise<void> {}\n  protected readonly x = 1;\n}\n"},
		{"t.cs", "using System;\npublic class A {\n  [HttpGet(\"/users\")]\n  public static async Task<int> Get() { return 1; }\n}\n"},
		{"t.py", "@app.route('/users', methods=['GET'])\n@cache.cached(60)\ndef list_users():\n    return []\n"},
		{"t.java", "import java.util.List;\npublic class A {\n  @Override\n  @SuppressWarnings(\"x\")\n  public static final int X = 1;\n}\n"},
	}
	for _, c := range cases {
		entry := grammars.DetectLanguage(c.file)
		lang := entry.Language()
		parser := NewExtractor().parserFor(Detect(c.file), lang)
		tree, err := parser.Parse([]byte(c.src))
		if err != nil || tree == nil {
			fmt.Println(c.file, "PARSE FAIL")
			continue
		}
		fmt.Println("=====", c.file)
		root := tree.RootNode()
		var walk func(n *gts.Node, d int)
		walk = func(n *gts.Node, d int) {
			ct := n.Type(lang)
			text := string([]byte(c.src)[int(n.StartByte()):int(n.EndByte())])
			if len(text) > 50 {
				text = text[:50]
			}
			if d > 0 {
				fmt.Printf("%*s%s %q\n", (d-1)*2, "", ct, text)
			}
			for i := 0; i < n.ChildCount(); i++ {
				walk(n.Child(i), d+1)
			}
		}
		walk(root, 0)
	}
}
