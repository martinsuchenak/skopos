package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/paularlott/cli"
)

// field reads an exported string or bool field of a flag or argument: the
// library's own accessors are unexported.
func field(v any, name string) reflect.Value {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}
	return rv.FieldByName(name)
}

func walkCommands(path string, c *cli.Command, visit func(path string, c *cli.Command)) {
	path = strings.TrimSpace(path + " " + c.Name)
	visit(path, c)
	for _, sub := range c.Commands {
		walkCommands(path, sub, visit)
	}
}

// TestHelpShowsEverything keeps `--help` the whole truth about a command:
// it is printed from the command's own declarations, so whatever a command
// takes has to be declared with a description, and nothing else may list
// options by hand (such a list goes out of date with the next flag).
func TestHelpShowsEverything(t *testing.T) {
	for _, root := range Commands() {
		walkCommands("skopos", root, func(path string, c *cli.Command) {
			if strings.TrimSpace(c.Usage) == "" {
				t.Errorf("%s: no description", path)
			}
			// A description that spells out the command line repeats the
			// flags and falls behind them.
			for _, text := range []string{c.Usage, c.Description} {
				if strings.Contains(text, "[--") {
					t.Errorf("%s: its text lists options by hand (%q): the help prints them from the flags", path, text)
				}
			}
			// Unnamed arguments show as "[args...]" in the help, which
			// says nothing about what to pass.
			if c.MinArgs != 0 || c.MaxArgs != 0 {
				t.Errorf("%s: takes unnamed arguments (MinArgs/MaxArgs): declare them in Arguments, with a description", path)
			}
			for _, a := range c.Arguments {
				name := field(a, "Name").String()
				if strings.TrimSpace(field(a, "Usage").String()) == "" {
					t.Errorf("%s: argument <%s> has no description", path, name)
				}
			}
			seen := map[string]bool{}
			for _, f := range c.Flags {
				name := field(f, "Name").String()
				if seen[name] {
					t.Errorf("%s: flag --%s is declared twice", path, name)
				}
				seen[name] = true
				if hidden := field(f, "Hidden"); hidden.IsValid() && hidden.Bool() {
					continue
				}
				if strings.TrimSpace(field(f, "Usage").String()) == "" {
					t.Errorf("%s: flag --%s has no description", path, name)
				}
			}
		})
	}
}

// TestNoHandWrittenUsageLines: a missing argument is reported by the library
// from the command's declared arguments. An error that spells out a usage
// line of its own names the options someone remembered when it was written.
func TestNoHandWrittenUsageLines(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `"usage: `) {
				t.Errorf("%s:%d: a usage line written by hand: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// globalFlags are the root command's flags (main.go), which every command
// takes.
var globalFlags = []string{"help", "config", "log-level", "log-format", "dns-server",
	"log-output-url", "log-output-format", "log-output-stream", "log-output-header"}

var (
	reDocCommand = regexp.MustCompile("(?:^|[\\s`(])skopos ([^`|;&#\\n]*)")
	reSubshell   = regexp.MustCompile(`\$\([^)]*\)`)
	reDocFlag    = regexp.MustCompile(`^--([a-z][a-z0-9-]*)`)
)

// TestDocsUseRealFlags reads every `skopos …` command line in the docs and
// checks that the command has the flags the line gives it: a documented flag
// the command refuses is found by a reader, at a prompt.
func TestDocsUseRealFlags(t *testing.T) {
	files, err := filepath.Glob("../docs/*/*.md")
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob("../docs/*/*/*.md")
	files = append(append(files, more...), "../README.md")
	if len(files) < 10 {
		t.Fatalf("found only %d documentation files: has the docs tree moved?", len(files))
	}
	root := &cli.Command{Name: "skopos", Commands: Commands()}
	checked := 0
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(src), "\n") {
			for _, m := range reDocCommand.FindAllStringSubmatch(line, -1) {
				words := strings.Fields(reSubshell.ReplaceAllString(m[1], ""))
				c, path := root, "skopos"
			descend:
				for len(words) > 0 {
					for _, sub := range c.Commands {
						if sub.Name == words[0] {
							c, path, words = sub, path+" "+sub.Name, words[1:]
							continue descend
						}
					}
					break
				}
				if c == root {
					continue // prose, or the root's own flags
				}
				has := map[string]bool{}
				for _, name := range globalFlags {
					has[name] = true
				}
				for _, f := range c.Flags {
					has[field(f, "Name").String()] = true
				}
				for _, w := range words {
					if fm := reDocFlag.FindStringSubmatch(w); fm != nil {
						checked++
						if !has[fm[1]] {
							t.Errorf("%s:%d: %s has no --%s", strings.TrimPrefix(file, "../"), n+1, path, fm[1])
						}
					}
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("checked only %d flags in the docs: the command lines are no longer found", checked)
	}
}

var (
	reFlagRead     = regexp.MustCompile(`\.Get(?:String|Bool|Int|Int64|Float64|StringSlice|Duration)\("([a-z0-9-]+)"\)`)
	reFlagDeclared = regexp.MustCompile(`Name:\s*"([a-z0-9-]+)"`)
	reFlagHelper   = regexp.MustCompile(`(?s)\nfunc (\w+)\([^)]*\) \[\]cli\.Flag \{\n(.*?)\n\}\n`)
)

// flagsFromElsewhere: flags a file reads that are declared where this scan
// of the source cannot see them.
var flagsFromElsewhere = map[string][]string{
	// The library's completion command declares its own query flags.
	"completion.go": {"command", "flag", "value-flags"},
	// The brief commands get their flags from the function their caller
	// passes in (the workspace's or the group's pipeline flags).
	"pipeline_briefs.go": {"server-url", "api-key"},
}

// TestFlagsReadAreDeclared: a flag a command reads but does not declare
// reads as empty and is refused on the command line as unknown, so the
// option exists in the code and nowhere a person can find or use it. For
// every file: each flag it reads is declared in it, or in a helper it calls
// that returns flags.
func TestFlagsReadAreDeclared(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	helpers := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src[name] = string(b)
		for _, m := range reFlagHelper.FindAllStringSubmatch(string(b), -1) {
			helpers[m[1]] = m[2]
		}
	}
	var declared func(text string, seen map[string]bool, into map[string]bool)
	declared = func(text string, seen map[string]bool, into map[string]bool) {
		for _, m := range reFlagDeclared.FindAllStringSubmatch(text, -1) {
			into[m[1]] = true
		}
		for helper, body := range helpers {
			if !seen[helper] && strings.Contains(text, helper+"(") {
				seen[helper] = true
				declared(body, seen, into)
			}
		}
	}
	reads := 0
	for name, text := range src {
		has := map[string]bool{}
		declared(text, map[string]bool{}, has)
		for _, g := range globalFlags {
			has[g] = true
		}
		for _, f := range flagsFromElsewhere[name] {
			has[f] = true
		}
		for _, m := range reFlagRead.FindAllStringSubmatch(text, -1) {
			reads++
			if !has[m[1]] {
				t.Errorf("%s reads --%s, which no command in it declares", name, m[1])
			}
		}
	}
	if reads < 200 {
		t.Fatalf("found only %d flag reads: the scan no longer matches the code", reads)
	}
}
