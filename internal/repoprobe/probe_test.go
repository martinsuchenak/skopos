package repoprobe

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func list(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// A Go module with a Taskfile and a bun frontend: the runner's check task,
// the kits its tasks call, setup for both toolchains, push from the id.
func TestProposeGoTaskfileAndFrontend(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":                           file("module github.com/acme/shop\n"),
		".golangci.yml":                    file("version: 2\n"),
		".github/workflows/ci.yml":         file("on: push\n"),
		"Taskfile.yml":                     file("version: '3'\nvars:\n  X: y\ntasks:\n  build:\n    cmds:\n      - go build ./...\n  lint:\n    cmds:\n      - golangci-lint run ./...\n  check:\n    deps: [lint]\n    cmds:\n      - task: test\n  test:\n    cmds:\n      - go test ./...\n  web:\n    dir: web\n    cmds:\n      - bun run build\n"),
		"web/package.json":                 file(`{"scripts":{"build":"vite build"}}`),
		"web/bun.lock":                     file("{}"),
		"web/node_modules/x/go.mod":        file("module x\n"),
		"examples/demo/package.json":       file(`{"scripts":{"test":"jest"}}`),
		"test/fixtures/package.json":       file(`{}`),
		"tools/a/b/package.json":           file(`{}`),
		".claude/worktrees/x/package.json": file(`{}`),
		"vendor/github.com/y/go.mod":       file("module y\n"),
		"cmd/tool/main.go":                 file("package main\n"),
	}
	p, err := Propose(fsys, "github.com/acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if got := list(p.Config.Checks); !slices.Equal(got, []string{"task check"}) {
		t.Fatalf("checks %v: the runner's check task, nothing from the toolchains", got)
	}
	if got := list(p.Config.Kits); !slices.Equal(got, []string{"bun", "github", "go", "golangci-lint", "task"}) {
		t.Fatalf("kits %v", got)
	}
	if got := list(p.Config.Setup); !slices.Equal(got, []string{"go mod download", "cd web && bun install --frozen-lockfile"}) {
		t.Fatalf("setup %v", got)
	}
	if p.Config.Push == nil || *p.Config.Push.Kind != "github" || *p.Config.Push.RemoteURL != "https://github.com/acme/shop.git" || *p.Config.Push.PullRequest != "draft" {
		t.Fatalf("push %+v", p.Config.Push)
	}
	if !slices.ContainsFunc(p.Notes, func(n string) bool {
		return strings.Contains(n, "examples/demo/package.json") && strings.Contains(n, "test/fixtures/package.json") && strings.Contains(n, "tools/a/b/package.json")
	}) {
		t.Fatalf("examples, fixtures and deep manifests are left out, and said so: %v", p.Notes)
	}
	if strings.Contains(strings.Join(p.Notes, " "), ".claude") {
		t.Fatalf("hidden folders are not read: %v", p.Notes)
	}
	if len(p.Why["checks"]) == 0 || len(p.Why["push.kind"]) == 0 {
		t.Fatalf("every field says why: %v", p.Why)
	}
	if _, err := wsconfig.Normalize(p.Config); err != nil {
		t.Fatalf("the proposal is a config the server takes: %v", err)
	}
}

// Without a runner, each toolchain gives its own checks.
func TestProposeToolchainChecks(t *testing.T) {
	cases := []struct {
		name   string
		fsys   fstest.MapFS
		kits   []string
		setup  []string
		checks []string
	}{
		{"go", fstest.MapFS{"go.mod": file("module m\n")},
			[]string{"go"}, []string{"go mod download"}, []string{"go build ./...", "go vet ./...", "go test ./..."}},
		{"npm", fstest.MapFS{"package.json": file(`{"scripts":{"test":"echo \"Error: no test specified\" && exit 1","lint":"eslint .","build":"tsc"}}`), "package-lock.json": file("{}")},
			[]string{"node"}, []string{"npm ci"}, []string{"npm run lint", "npm run build"}},
		{"composer", fstest.MapFS{"composer.json": file(`{"scripts":{"test":"phpunit"}}`), "phpstan.neon": file("")},
			[]string{"php"}, []string{"composer install --no-interaction --no-progress"}, []string{"composer test", "vendor/bin/phpstan analyse --no-progress"}},
		{"uv", fstest.MapFS{"pyproject.toml": file("[tool.ruff]\nline-length = 100\n"), "uv.lock": file(""), "tests/test_a.py": file("def test_a(): pass\n")},
			[]string{"python"}, []string{"uv sync"}, []string{"uv run ruff check .", "uv run pytest"}},
		{"dotnet", fstest.MapFS{"Mail.slnx": file(""), "src/Mail/Mail.csproj": file(""), "src/Mail.Tests/Mail.Tests.csproj": file("")},
			[]string{"dotnet"}, []string{"dotnet restore"}, []string{"dotnet build --no-restore", "dotnet test --no-build"}},
		{"scriptling", fstest.MapFS{"routes.py": file("import scriptling.runtime.http as http\n"), "lib/util.py": file("import os\n")},
			[]string{"scriptling"}, nil, nil},
		{"loose python", fstest.MapFS{"scripts/x.py": file("import os\n")},
			nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Propose(c.fsys, "gitea.acme.test/acme/shop")
			if err != nil {
				t.Fatal(err)
			}
			if got := list(p.Config.Kits); !slices.Equal(got, c.kits) {
				t.Fatalf("kits %v, want %v", got, c.kits)
			}
			if got := list(p.Config.Setup); !slices.Equal(got, c.setup) {
				t.Fatalf("setup %v, want %v", got, c.setup)
			}
			if got := list(p.Config.Checks); !slices.Equal(got, c.checks) {
				t.Fatalf("checks %v, want %v", got, c.checks)
			}
			if p.Config.Push != nil {
				t.Fatal("no push is proposed off github.com")
			}
		})
	}
}

func TestMakeTargetsAndTaskNames(t *testing.T) {
	got := makeTargets(".PHONY: test lint\nCC := gcc\nall: build\nbuild:\n\tgo build\ntest: build\n\tgo test\nlint:\n\tgolangci-lint run\nX=1\n")
	if !slices.Equal(got, []string{"all", "build", "test", "lint"}) {
		t.Fatalf("make targets %v", got)
	}
	got = taskNames("version: '3'\ntasks:\n  default:\n    cmds:\n      - task: build\n  \"fmt:check\":\n    cmds: [gofmt -l .]\n  test:race:\n    cmds:\n      - go test -race ./...\nincludes:\n  docs: ./docs\n")
	if !slices.Equal(got, []string{"default", "fmt:check", "test:race"}) {
		t.Fatalf("task names %v", got)
	}
}

// Fill adds only the fields neither the workspace nor a group sets.
func TestFillNeverReplaces(t *testing.T) {
	p, err := Propose(fstest.MapFS{"go.mod": file("module m\n")}, "github.com/acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	own := wsconfig.Config{Checks: &[]string{"make test"}}
	out, added, err := p.Fill(own, map[string]string{"push.kind": "group:work"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(added, ",") != "kits,push.pull_request,push.remote_url,setup" {
		t.Fatalf("added %v", added)
	}
	if list(out.Checks)[0] != "make test" || out.Push == nil || out.Push.Kind != nil {
		t.Fatalf("kept the workspace's checks and the group's push kind: %+v", out)
	}
	if _, added, _ = p.Fill(out, map[string]string{"push.kind": "group:work"}); len(added) != 0 {
		t.Fatalf("nothing left to add: %v", added)
	}
}

// ProposeTracked reads only the files git tracks: a downloaded copy or a
// generated folder in the checkout is not the repository's project.
func TestProposeTracked(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":                         file("module m\n"),
		"Makefile":                       file("test:\n\tgo test ./...\n"),
		"PHP-Parser-5.6.1/composer.json": file(`{"scripts":{"test":"phpunit"}}`),
		"web/package.json":               file(`{"scripts":{"build":"vite build"}}`),
		"web/package-lock.json":          file("{}"),
		"node_modules/x/package.json":    file(`{}`),
		".claude/x/package.json":         file(`{}`),
	}
	tracked := []string{"go.mod", "Makefile", "web/package.json", "web/package-lock.json", "node_modules/x/package.json", ".claude/x/package.json"}
	p, err := ProposeTracked(fsys, tracked, "github.com/acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if got := list(p.Config.Setup); !slices.Equal(got, []string{"go mod download", "cd web && npm ci"}) {
		t.Fatalf("setup %v: the untracked copy, dependencies and hidden folders are not read", got)
	}
	if slices.Contains(list(p.Config.Kits), "php") {
		t.Fatal("no php kit from an untracked copy")
	}
	// Every file counts in a plain read.
	if p, _ = Propose(fsys, "github.com/acme/shop"); !slices.Contains(list(p.Config.Kits), "php") {
		t.Fatal("Propose reads the untracked copy")
	}
}

// A manifest deeper than one folder counts when the task runner works in
// its folder.
func TestProposeRunnerFolder(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":                  file("module m\n"),
		"Makefile":                file("web:\n\tcd embed/web && bun install && bun run build\ntest:\n\tgo test ./...\n"),
		"embed/web/package.json":  file(`{}`),
		"other/deep/package.json": file(`{}`),
	}
	p, err := Propose(fsys, "github.com/acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if got := list(p.Config.Setup); !slices.Equal(got, []string{"go mod download", "cd embed/web && bun install"}) {
		t.Fatalf("setup %v: no lockfile, and the Makefile installs with bun", got)
	}
	if !slices.Contains(p.Why["setup"], "cd embed/web && bun install: package.json in embed/web, no lockfile") {
		t.Fatalf("why: %v", p.Why["setup"])
	}
}

// mago.toml proposes the mago kit and leaves the check to the person.
func TestProposeMago(t *testing.T) {
	p, err := Propose(fstest.MapFS{
		"mago.toml":             file("version = \"1\"\n"),
		"laravel/composer.json": file(`{}`),
	}, "github.com/acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(list(p.Config.Kits), "mago") || slices.ContainsFunc(list(p.Config.Checks), func(c string) bool { return strings.Contains(c, "mago") }) {
		t.Fatalf("kits %v, checks %v", list(p.Config.Kits), list(p.Config.Checks))
	}
	if !slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "add `mago lint`") }) {
		t.Fatalf("notes %v", p.Notes)
	}
	if _, ok := wsconfig.KitByName("mago"); !ok {
		t.Fatal("the catalogue has the mago kit")
	}
}
