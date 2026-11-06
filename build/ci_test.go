package build

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestWorkflowActionsArePinned keeps every GitHub Action pinned to a commit
// hash: a tag or branch can be moved to other code. Dependabot updates the
// pins (.github/dependabot.yml).
func TestWorkflowActionsArePinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	uses := regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)`)
	pinned := regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range uses.FindAllStringSubmatch(string(data), -1) {
			if !pinned.MatchString(m[1]) {
				t.Errorf("%s: %s is not pinned to a commit hash", filepath.Base(f), m[1])
			}
		}
	}
}
