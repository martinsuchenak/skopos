// Package repoprobe proposes a workspace's configuration from its
// repository's files: the kits its toolchains need, the setup that installs
// its dependencies, the checks its own task runner or toolchain defines, and
// the push block its workspace id implies. It reads files and decides
// nothing: a person saves the proposal (a kit is a grant).
package repoprobe

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Proposal is a workspace config proposed from a repository, with the
// reasons for each field (keyed by the field's flattened name: kits, setup,
// checks, push.kind …) and notes on what it could not decide.
type Proposal struct {
	Config wsconfig.Config     `json:"config"`
	Why    map[string][]string `json:"why"`
	Notes  []string            `json:"notes,omitempty"`
}

// maxDepth: how deep the probe looks for a project (a frontend in web/, a
// .NET project in src/Name/).
const maxDepth = 3

// skipDirs are never looked into: dependencies, build output, caches.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"dist": true, "build": true, "bin": true, "obj": true, "target": true,
	"__pycache__": true, ".next": true, ".cache": true, "tmp": true,
}

type probe struct {
	fsys  fs.FS
	files []string // slash paths, depth ≤ maxDepth
	// runnerText: the task runner's file, once runner() found one.
	runnerText string
	dirs       map[string]bool
	kits       map[string]bool
	setup      []string
	check      []string
	p          *Proposal
}

func (pr *probe) why(field, reason string) {
	if !slices.Contains(pr.p.Why[field], reason) {
		pr.p.Why[field] = append(pr.p.Why[field], reason)
	}
}

func (pr *probe) kit(name, reason string) {
	pr.kits[name] = true
	pr.why("kits", name+": "+reason)
}

func (pr *probe) note(s string) {
	if !slices.Contains(pr.p.Notes, s) {
		pr.p.Notes = append(pr.p.Notes, s)
	}
}

func (pr *probe) has(p string) bool { return slices.Contains(pr.files, p) }

// notProject: folders whose manifests are not the repository's own project
// (examples, fixtures, tests, documentation sites): no setup or checks are
// proposed for what is in them.
var notProject = map[string]bool{
	"example": true, "examples": true, "sample": true, "samples": true, "demo": true, "demos": true,
	"fixture": true, "fixtures": true, "__fixtures__": true, "testdata": true,
	"test": true, "tests": true, "__tests__": true, "docs": true,
}

// projects keeps the manifests that belong to the repository's own project:
// at the root, one folder down (web/, frontend/), or a folder the task
// runner works in, outside the folders notProject names. It notes the ones
// it leaves out.
func (pr *probe) projects(files []string) []string {
	var keep, left []string
	for _, f := range files {
		dir := path.Dir(f)
		// Deeper than one folder only where the task runner works in it
		// (cd embed/web && bun install).
		own := strings.Count(f, "/") <= 1 || (pr.runnerText != "" && strings.Contains(pr.runnerText, dir))
		for _, seg := range strings.Split(dir, "/") {
			if notProject[seg] {
				own = false
			}
		}
		if own {
			keep = append(keep, f)
		} else {
			left = append(left, f)
		}
	}
	if len(left) > 0 {
		pr.note("left out (not the repository's own project, or deeper than one folder): " + strings.Join(left, ", "))
	}
	return keep
}

// head reads the first n bytes of a file.
func (pr *probe) head(p string, n int64) string {
	f, err := pr.fsys.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}

func (pr *probe) read(p string) string {
	b, err := fs.ReadFile(pr.fsys, p)
	if err != nil {
		return ""
	}
	return string(b)
}

// matching lists the files whose base name matches one of names, shallowest
// first.
func (pr *probe) matching(names ...string) []string {
	var out []string
	for _, f := range pr.files {
		for _, n := range names {
			if ok, _ := path.Match(n, path.Base(f)); ok {
				out = append(out, f)
				break
			}
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return strings.Count(a, "/") - strings.Count(b, "/") })
	return out
}

// skipDir: a folder the probe does not look into — dependencies, build
// output, caches, and hidden folders (tools' own: .claude, .idea) other than
// .github, which is read for its workflows.
func skipDir(name string) bool {
	return skipDirs[name] || (strings.HasPrefix(name, ".") && name != ".github")
}

// Propose reads the repository in fsys (its default branch, checked out)
// and proposes the config for workspace: every file in it counts.
func Propose(fsys fs.FS, workspace string) (*Proposal, error) {
	return propose(fsys, nil, workspace)
}

// ProposeTracked is Propose over the files git tracks (slash paths, as `git
// ls-files` lists them): what a worker's clone holds. Files a checkout has
// besides (downloaded copies, generated folders, data) are not read.
func ProposeTracked(fsys fs.FS, tracked []string, workspace string) (*Proposal, error) {
	if tracked == nil {
		tracked = []string{}
	}
	return propose(fsys, tracked, workspace)
}

func propose(fsys fs.FS, tracked []string, workspace string) (*Proposal, error) {
	pr := &probe{fsys: fsys, dirs: map[string]bool{}, kits: map[string]bool{}, p: &Proposal{Why: map[string][]string{}}}
	if tracked != nil {
	files:
		for _, f := range tracked {
			segs := strings.Split(f, "/")
			if len(segs)-1 > maxDepth {
				continue
			}
			for _, seg := range segs[:len(segs)-1] {
				if skipDir(seg) {
					continue files
				}
			}
			pr.files = append(pr.files, f)
			for d := path.Dir(f); d != "."; d = path.Dir(d) {
				pr.dirs[d] = true
			}
		}
		return pr.finish(workspace), nil
	}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil
		}
		depth := strings.Count(p, "/")
		if d.IsDir() {
			if p != "." && (skipDir(d.Name()) || depth >= maxDepth) {
				return fs.SkipDir
			}
			pr.dirs[p] = true
			return nil
		}
		pr.files = append(pr.files, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pr.finish(workspace), nil
}

// finish reads the files found and builds the proposal.
func (pr *probe) finish(workspace string) *Proposal {
	runnerChecks := pr.runner()
	pr.goModule()
	pr.node(runnerChecks)
	pr.php(runnerChecks)
	pr.mago()
	pr.python(runnerChecks)
	pr.dotnet(runnerChecks)
	if pr.dirs[".github/workflows"] {
		pr.kit("github", "the repository has GitHub workflows (an update looks up actions' versions on GitHub)")
	}
	pr.push(workspace)

	if len(pr.kits) > 0 {
		var kits []string
		for _, k := range wsconfig.Kits() {
			if pr.kits[k.Name] {
				kits = append(kits, k.Name)
			}
		}
		pr.p.Config.Kits = &kits
	}
	if len(pr.setup) > 0 {
		pr.p.Config.Setup = &pr.setup
	}
	if len(pr.check) > 0 {
		pr.p.Config.Checks = &pr.check
	}
	return pr.p
}

// runner reads the repository's task runner (Taskfile, else Makefile) and
// takes its checks from it: a check (or ci) task when there is one, else
// its vet, lint and test tasks. It reports whether it set the checks.
func (pr *probe) runner() bool {
	var file, cmd string
	var names []string
	for _, f := range []string{"Taskfile.yml", "Taskfile.yaml", "Taskfile.dist.yml", "Taskfile.dist.yaml"} {
		if pr.has(f) {
			file, cmd, names = f, "task", taskNames(pr.read(f))
			pr.kit("task", f+" is the repository's task runner")
			break
		}
	}
	if file == "" {
		for _, f := range []string{"Makefile", "makefile", "GNUmakefile"} {
			if pr.has(f) {
				file, cmd, names = f, "make", makeTargets(pr.read(f))
				pr.kit("make", f+" is the repository's task runner")
				break
			}
		}
	}
	if file == "" {
		return false
	}
	pr.runnerText = pr.read(file)
	// The tools the runner's tasks call need their kits too.
	text := strings.ToLower(pr.runnerText)
	for _, m := range []struct{ word, kit string }{
		{"golangci-lint", "golangci-lint"}, {"bun ", "bun"}, {"bunx ", "bun"}, {"npm ", "node"}, {"npx ", "node"},
		{"composer ", "php"}, {"uv ", "python"}, {"pytest", "python"}, {"dotnet ", "dotnet"}, {"scriptling", "scriptling"},
	} {
		if strings.Contains(text, m.word) {
			pr.kit(m.kit, file+" calls "+strings.TrimSpace(m.word))
		}
	}
	var picked []string
	switch {
	case slices.Contains(names, "check"):
		picked = []string{"check"}
	case slices.Contains(names, "ci"):
		picked = []string{"ci"}
	default:
		for _, n := range []string{"vet", "lint", "test"} {
			if slices.Contains(names, n) {
				picked = append(picked, n)
			}
		}
	}
	if len(picked) == 0 {
		pr.note(file + " has no check, ci, vet, lint or test task: the checks come from the toolchains")
		return false
	}
	for _, n := range picked {
		pr.check = append(pr.check, cmd+" "+n)
	}
	what := "a " + picked[0] + " task"
	if len(picked) > 1 {
		what = "its " + strings.Join(picked, ", ") + " tasks"
	}
	pr.why("checks", file+" has "+what)
	return true
}

var reTaskName = regexp.MustCompile(`^(\s+)(["']?[A-Za-z0-9_][A-Za-z0-9_:.-]*["']?)\s*:`)

// taskNames lists a Taskfile's task names: the keys one level under tasks:.
// A line scan, not a YAML parser: a Taskfile's tasks are plain keys.
func taskNames(text string) []string {
	var names []string
	in, indent := false, ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			in, indent = strings.HasPrefix(line, "tasks:"), ""
			continue
		}
		if !in {
			continue
		}
		m := reTaskName.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if indent == "" {
			indent = m[1]
		}
		if m[1] == indent {
			names = append(names, strings.Trim(m[2], `"'`))
		}
	}
	return names
}

var reMakeTarget = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:([^=]|$)`)

// makeTargets lists a Makefile's targets.
func makeTargets(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		if m := reMakeTarget.FindStringSubmatch(line); m != nil && !strings.HasPrefix(m[1], ".") {
			names = append(names, m[1])
		}
	}
	return names
}

func (pr *probe) goModule() {
	if !pr.has("go.mod") {
		if mods := pr.projects(pr.matching("go.mod")); len(mods) > 0 {
			pr.note("a Go module in " + path.Dir(mods[0]) + ", not at the root: setup and checks for it are left to you")
			pr.kit("go", path.Dir(mods[0])+" is a Go module")
		}
		return
	}
	pr.kit("go", "go.mod at the root")
	pr.setup = append(pr.setup, "go mod download")
	pr.why("setup", "go mod download: go.mod at the root")
	for _, f := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"} {
		if pr.has(f) {
			pr.kit("golangci-lint", f+" configures golangci-lint")
		}
	}
	if len(pr.check) == 0 {
		pr.check = append(pr.check, "go build ./...", "go vet ./...", "go test ./...")
		pr.why("checks", "go build, vet and test: a Go module without a task runner's checks")
	}
}

// node proposes setup and checks for each package.json (the root and up to
// two folders down), by its lockfile.
func (pr *probe) node(runnerChecks bool) {
	for _, pkg := range pr.projects(pr.matching("package.json")) {
		dir := path.Dir(pkg)
		in := func(f string) bool { return pr.has(path.Join(dir, f)) }
		prefix := ""
		where := "the root"
		if dir != "." {
			prefix, where = "cd "+dir+" && ", dir
		}
		var manager, install string
		switch {
		case in("bun.lock") || in("bun.lockb"):
			manager, install = "bun", "bun install --frozen-lockfile"
			pr.kit("bun", "bun's lockfile in "+where)
			if in("package-lock.json") {
				pr.note(where + " has both bun's and npm's lockfile: bun was taken")
			}
		case in("pnpm-lock.yaml"):
			manager, install = "pnpm", "pnpm install --frozen-lockfile"
			pr.kit("node", "pnpm's lockfile in "+where)
			pr.note("pnpm is in no kit: add Bash(pnpm:*) to the stages that run it")
		case in("yarn.lock") && !in("package-lock.json"):
			manager, install = "yarn", "yarn install --frozen-lockfile"
			pr.kit("node", "yarn's lockfile in "+where)
			pr.note("yarn is in no kit: add Bash(yarn:*) to the stages that run it")
		case in("package-lock.json"):
			manager, install = "npm", "npm ci"
			pr.kit("node", "npm's lockfile in "+where)
			if in("yarn.lock") {
				pr.note(where + " has both npm's and yarn's lockfile: npm was taken")
			}
		case strings.Contains(strings.ToLower(pr.runnerText), "bun "):
			// No lockfile, but the task runner installs it with bun.
			manager, install = "bun", "bun install"
			pr.kit("bun", "the task runner uses bun for "+where)
			pr.note(where + " has no lockfile: setup runs bun install, which may resolve other versions on each worker")
		default:
			manager, install = "npm", "npm install"
			pr.kit("node", "package.json in "+where)
			pr.note(where + " has no lockfile: setup runs npm install, which may resolve other versions on each worker")
		}
		reason := manager + "'s lockfile in " + where
		if install == "npm install" || install == "bun install" {
			reason = "package.json in " + where + ", no lockfile"
		}
		pr.setup = append(pr.setup, prefix+install)
		pr.why("setup", prefix+install+": "+reason)
		if runnerChecks {
			continue
		}
		var doc struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal([]byte(pr.read(pkg)), &doc) != nil {
			pr.note(pkg + " could not be read as JSON")
			continue
		}
		for _, s := range []string{"lint", "typecheck", "test", "build"} {
			body, ok := doc.Scripts[s]
			if !ok || strings.Contains(body, "no test specified") {
				continue
			}
			run := manager + " run " + s
			pr.check = append(pr.check, prefix+run)
			pr.why("checks", prefix+run+": a "+s+" script in "+pkg)
		}
	}
}

func (pr *probe) php(runnerChecks bool) {
	files := pr.projects(pr.matching("composer.json"))
	if len(files) == 0 {
		return
	}
	dir := path.Dir(files[0])
	prefix, where := "", "the root"
	if dir != "." {
		prefix, where = "cd "+dir+" && ", dir
	}
	pr.kit("php", "composer.json in "+where)
	install := prefix + "composer install --no-interaction --no-progress"
	pr.setup = append(pr.setup, install)
	pr.why("setup", install+": composer.json in "+where)
	if runnerChecks {
		return
	}
	var doc struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	_ = json.Unmarshal([]byte(pr.read(files[0])), &doc)
	in := func(f string) bool { return pr.has(path.Join(dir, f)) }
	switch {
	case doc.Scripts["test"] != nil:
		pr.check = append(pr.check, prefix+"composer test")
		pr.why("checks", prefix+"composer test: a test script in "+files[0])
	case in("phpunit.xml") || in("phpunit.xml.dist"):
		pr.check = append(pr.check, prefix+"vendor/bin/phpunit")
		pr.why("checks", prefix+"vendor/bin/phpunit: PHPUnit is configured in "+where)
	}
	if in("phpstan.neon") || in("phpstan.neon.dist") {
		pr.check = append(pr.check, prefix+"vendor/bin/phpstan analyse --no-progress")
		pr.why("checks", prefix+"vendor/bin/phpstan analyse: PHPStan is configured in "+where)
	}
}

// mago proposes the mago kit for a repository that configures it
// (mago.toml), and leaves the check to the person: on a large repository
// a full mago run is a choice, not a default.
func (pr *probe) mago() {
	files := pr.projects(pr.matching("mago.toml"))
	if len(files) == 0 {
		return
	}
	pr.kit("mago", files[0]+" configures mago")
	pr.note(files[0] + " configures mago: add `mago lint` (or `mago analyze`) to the checks if the workers should run it")
}

// python tells a scriptling repository (scripts that import scriptling's
// libraries) from a Python one, and proposes setup and checks for a Python
// project with a lockfile or requirements.
func (pr *probe) python(runnerChecks bool) {
	scripts := pr.matching("*.py")
	scriptling := 0
	for _, f := range scripts {
		if importsScriptling(pr.head(f, scriptHead)) {
			scriptling++
		}
	}
	if scriptling > 0 {
		pr.kit("scriptling", "scripts import scriptling's libraries")
		if !pr.has("pyproject.toml") && !pr.has("requirements.txt") {
			pr.note("scriptling scripts: no check is proposed; add the command that runs the repository's tests")
			return
		}
	}
	pyproject := pr.read("pyproject.toml")
	tests := pr.dirs["tests"] || pr.dirs["test"] || strings.Contains(pyproject, "pytest")
	ruff := strings.Contains(pyproject, "[tool.ruff") || pr.has("ruff.toml") || pr.has(".ruff.toml")
	switch {
	case pr.has("uv.lock"):
		pr.kit("python", "uv.lock at the root")
		pr.setup = append(pr.setup, "uv sync")
		pr.why("setup", "uv sync: uv.lock at the root")
		if runnerChecks {
			return
		}
		if ruff {
			pr.check = append(pr.check, "uv run ruff check .")
			pr.why("checks", "uv run ruff check: ruff is configured")
		}
		if tests {
			pr.check = append(pr.check, "uv run pytest")
			pr.why("checks", "uv run pytest: the repository has tests")
		}
	case pr.has("requirements.txt"):
		pr.kit("python", "requirements.txt at the root")
		cmd := "python3 -m venv .venv && .venv/bin/pip install -r requirements.txt"
		pr.setup = append(pr.setup, cmd)
		pr.why("setup", "a virtual environment in .venv from requirements.txt")
		pr.note("setup makes .venv in the clone: keep it in .gitignore")
		if !runnerChecks && tests {
			pr.check = append(pr.check, ".venv/bin/python -m pytest")
			pr.why("checks", ".venv/bin/python -m pytest: the repository has tests")
		}
	case pyproject != "":
		pr.kit("python", "pyproject.toml at the root")
		pr.note("pyproject.toml without uv.lock or requirements.txt: setup is left to you")
	case len(scripts) > scriptling:
		pr.note("Python scripts without pyproject.toml, uv.lock or requirements.txt: no kit is proposed for them; add the python kit if the stages run them")
	}
}

// scriptHead: how much of a script is read for its imports.
const scriptHead = 8192

// importsScriptling: a script that imports one of scriptling's own
// libraries (import scriptling.…, from scriptling… import).
func importsScriptling(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "import scriptling") || strings.HasPrefix(line, "from scriptling") {
			return true
		}
	}
	return false
}

func (pr *probe) dotnet(runnerChecks bool) {
	sln := pr.matching("*.sln", "*.slnx")
	projects := pr.matching("*.csproj", "*.fsproj", "*.vbproj")
	if len(sln) == 0 && len(projects) == 0 {
		return
	}
	var target, where string
	switch {
	case len(sln) > 0:
		if path.Dir(sln[0]) != "." || len(sln) > 1 {
			target = " " + sln[0]
		}
		where = sln[0]
		if len(sln) > 1 {
			pr.note("several solutions: " + sln[0] + " was taken")
		}
	case len(projects) == 1:
		if path.Dir(projects[0]) != "." {
			target = " " + projects[0]
		}
		where = projects[0]
	default:
		pr.kit("dotnet", ".NET projects in the repository")
		pr.note("several .NET projects and no solution: setup and checks are left to you")
		return
	}
	pr.kit("dotnet", where+" is a .NET build")
	pr.setup = append(pr.setup, "dotnet restore"+target)
	pr.why("setup", "dotnet restore: "+where)
	if runnerChecks {
		return
	}
	pr.check = append(pr.check, "dotnet build --no-restore"+target)
	pr.why("checks", "dotnet build: "+where)
	for _, p := range projects {
		if strings.Contains(strings.ToLower(path.Base(p)), "test") {
			pr.check = append(pr.check, "dotnet test --no-build"+target)
			pr.why("checks", "dotnet test: a test project, "+p)
			break
		}
	}
}

// push proposes the push block a workspace on GitHub takes: its https URL
// and draft pull requests. The token stays each worker's (push.token_env).
func (pr *probe) push(workspace string) {
	owner, ok := strings.CutPrefix(workspace, "github.com/")
	if !ok || strings.Count(owner, "/") != 1 {
		pr.note("push: only a workspace on github.com is proposed one; set push.kind and push.remote_url for this forge")
		return
	}
	kind, remote, pull := "github", "https://"+workspace+".git", "draft"
	pr.p.Config.Push = &wsconfig.Push{Kind: &kind, RemoteURL: &remote, PullRequest: &pull}
	pr.why("push.kind", "the workspace is on github.com")
	pr.why("push.remote_url", "the workspace's https address")
	pr.why("push.pull_request", "a draft pull request for each branch")
	pr.note("each worker sets the push token's variable itself: push.token_env in its file (for example GH_TOKEN), with write access to the repository")
}

// Fill lays the proposal's fields over own (the workspace's stored config)
// where neither own nor anything else that applies (sources: the effective
// config's field → owner) sets them. It never replaces a field someone set.
// It returns the result and the fields it added.
func (p *Proposal) Fill(own wsconfig.Config, sources map[string]string) (wsconfig.Config, []string, error) {
	fields := own.Fields()
	var added []string
	for name, v := range p.Config.Fields() {
		if _, set := fields[name]; set {
			continue
		}
		if _, set := sources[name]; set {
			continue
		}
		fields[name] = v
		added = append(added, name)
	}
	slices.Sort(added)
	if len(added) == 0 {
		return own, nil, nil
	}
	out, err := wsconfig.FromFields(fields)
	return out, added, err
}
