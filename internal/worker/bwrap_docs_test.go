package worker

import (
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// docsBwrapWrapper reads the bwrap stage wrapper printed in
// docs/deployment/worker.md (between the bwrap-profile markers), so the
// profile the docs show is the one the tests run.
func docsBwrapWrapper(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/deployment/worker.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	a, b := strings.Index(doc, "<!-- bwrap-profile:begin -->"), strings.Index(doc, "<!-- bwrap-profile:end -->")
	if a < 0 || b < a {
		t.Fatal("docs/deployment/worker.md: the bwrap profile markers are gone")
	}
	block := doc[a:b]
	block = block[strings.Index(block, "```toml")+len("```toml") : strings.LastIndex(block, "```")]
	var file struct {
		Worker struct {
			Profiles map[string]map[string]*StageProfile `toml:"profiles"`
		} `toml:"worker"`
	}
	if _, err := toml.Decode(block, &file); err != nil {
		t.Fatalf("the documented bwrap profile is not valid TOML: %v", err)
	}
	prof := file.Worker.Profiles["bwrap"]["implement"]
	if prof == nil || len(prof.Wrapper) == 0 {
		t.Fatal("the documented bwrap profile has no wrapper")
	}
	return prof.Wrapper
}

// The documented profile is a valid stage profile, binds everything a run
// needs and nothing wider, and yields an isolated executor.
func TestDocsBwrapProfile(t *testing.T) {
	wrapper := docsBwrapWrapper(t)
	joined := strings.Join(wrapper, " ")
	for _, ph := range []string{"{dir}", "{gitdir}", "{login}", "{rundir}", "{cli}"} {
		if !strings.Contains(joined, ph) {
			t.Errorf("the profile must bind %s", ph)
		}
	}
	for i, a := range wrapper {
		if (a == "--bind" || a == "--ro-bind" || a == "--ro-bind-try") && i+1 < len(wrapper) {
			switch wrapper[i+1] {
			case "/", "/etc", "/home", "/root", "/var", "/opt":
				t.Errorf("the profile binds %s whole", wrapper[i+1])
			}
		}
	}
	c := &Config{Profiles: map[string]map[string]*StageProfile{"bwrap": {"implement": {Wrapper: wrapper}}},
		Repos: map[string]*Repo{"ws": {Path: "/clones/ws", Profile: "bwrap"}}}
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := c.ExecutorFor("ws", "implement", "claude"); !strings.HasPrefix(got, "wrapper:bwrap:") {
		t.Fatalf("executor: %q", got)
	}
}
