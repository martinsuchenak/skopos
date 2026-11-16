package wsconfig

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Kits are ready-made parts of a stage block for one toolchain: the
// commands a stage may run, the hosts those commands download from, and
// the cache folders they write. A stage block names kits (StageCaps.Kits);
// the worker turns them into allowed tools and Claude Code's sandbox
// settings for its own host, creating the cache folders, and checks before
// it claims work that the commands are on the run's PATH.
//
// A kit's grants run only inside the sandbox: naming a kit turns the
// sandbox on for the stage, with commands refused any host outside the
// allowlist and no way to run outside the sandbox.

// Kit is one toolchain's part of a stage block.
type Kit struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	// Tools: allowed tool patterns the kit adds (Bash(cmd:*)).
	Tools []string `json:"tools"`
	// Commands: the programs the tools run, checked on the run's PATH.
	Commands []string `json:"commands"`
	// Hosts: what the sandbox lets the commands reach.
	Hosts []string `json:"hosts,omitempty"`
	// Write: folders the commands write outside the worktree. "~" is the
	// home folder, {cache} the user's cache folder, {gopath} and {gocache}
	// what `go env` says. The worker creates them: the sandbox opens only
	// a folder that already exists.
	Write []string `json:"write,omitempty"`
}

// githubHosts serve modules, actions and release downloads fetched from
// GitHub.
var githubHosts = []string{"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"}

// kits is the catalogue, by name.
var kits = []Kit{
	{
		Name: "go", Title: "Go: go and gofmt, the module proxy and checksum database, the module and build caches",
		Tools:    []string{"Bash(go:*)", "Bash(gofmt:*)"},
		Commands: []string{"go", "gofmt"},
		Hosts:    append([]string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "golang.org", "go.dev", "gopkg.in", "go.googlesource.com"}, githubHosts...),
		Write:    []string{"{gopath}/pkg", "{gocache}"},
	},
	{
		Name: "golangci-lint", Title: "golangci-lint and its cache",
		Tools:    []string{"Bash(golangci-lint:*)"},
		Commands: []string{"golangci-lint"},
		Write:    []string{"{cache}/golangci-lint"},
	},
	{
		Name: "node", Title: "Node: node, npm and npx, the npm registry and npm's cache",
		Tools:    []string{"Bash(node:*)", "Bash(npm:*)", "Bash(npx:*)"},
		Commands: []string{"node", "npm", "npx"},
		Hosts:    []string{"registry.npmjs.org"},
		Write:    []string{"~/.npm"},
	},
	{
		Name: "bun", Title: "Bun: bun and bunx, the npm registry and bun's cache",
		Tools:    []string{"Bash(bun:*)", "Bash(bunx:*)"},
		Commands: []string{"bun"},
		Hosts:    []string{"registry.npmjs.org"},
		Write:    []string{"~/.bun/install/cache"},
	},
	{
		Name: "php", Title: "PHP: php and composer, Packagist and composer's cache",
		Tools:    []string{"Bash(php:*)", "Bash(composer:*)", "Bash(vendor/bin/*:*)"},
		Commands: []string{"php", "composer"},
		Hosts:    append([]string{"repo.packagist.org", "packagist.org"}, githubHosts...),
		Write:    []string{"{cache}/composer"},
	},
	{
		Name: "python", Title: "Python: uv, pip, python, pytest, ruff and mypy, PyPI and the uv and pip caches",
		Tools:    []string{"Bash(uv:*)", "Bash(uvx:*)", "Bash(python:*)", "Bash(python3:*)", "Bash(pip:*)", "Bash(pip3:*)", "Bash(pytest:*)", "Bash(ruff:*)", "Bash(mypy:*)"},
		Commands: []string{"python3"},
		Hosts:    []string{"pypi.org", "files.pythonhosted.org"},
		Write:    []string{"~/.cache/uv", "{cache}/pip"},
	},
	{
		Name: "dotnet", Title: ".NET: the dotnet CLI, NuGet and the NuGet package cache",
		Tools:    []string{"Bash(dotnet:*)"},
		Commands: []string{"dotnet"},
		Hosts:    []string{"api.nuget.org", "globalcdn.nuget.org", "dotnetcli.azureedge.net", "builds.dotnet.microsoft.com"},
		Write:    []string{"~/.nuget/packages", "~/.dotnet", "~/.local/share/NuGet"},
	},
	{
		Name: "scriptling", Title: "Scriptling: the scriptling interpreter",
		Tools:    []string{"Bash(scriptling:*)"},
		Commands: []string{"scriptling"},
	},
	{
		Name: "task", Title: "Task: the task runner (go-task)",
		Tools:    []string{"Bash(task:*)"},
		Commands: []string{"task"},
	},
	{
		Name: "make", Title: "Make",
		Tools:    []string{"Bash(make:*)"},
		Commands: []string{"make"},
	},
	{
		Name: "github", Title: "GitHub lookups: git ls-remote, and GitHub's hosts for actions and release downloads",
		Tools:    []string{"Bash(git ls-remote:*)"},
		Commands: []string{"git"},
		Hosts:    githubHosts,
	},
}

// Kits returns the catalogue, by name.
func Kits() []Kit {
	out := slices.Clone(kits)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KitByName returns a kit of the catalogue.
func KitByName(name string) (Kit, bool) {
	for _, k := range kits {
		if k.Name == name {
			return k, true
		}
	}
	return Kit{}, false
}

// kitNames lists the catalogue's names, sorted.
func kitNames() []string {
	out := make([]string, len(kits))
	for i, k := range kits {
		out[i] = k.Name
	}
	sort.Strings(out)
	return out
}

// normalizeKits checks a block's kit names and returns them deduplicated,
// in the order given.
func normalizeKits(in []string) ([]string, error) {
	var out []string
	for _, n := range in {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || slices.Contains(out, n) {
			continue
		}
		if _, ok := KitByName(n); !ok {
			return nil, fmt.Errorf("%q is not a kit (%s)", n, strings.Join(kitNames(), ", "))
		}
		out = append(out, n)
	}
	return out, nil
}
