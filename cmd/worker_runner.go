package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/mattn/go-isatty"
	"github.com/paularlott/cli"
)

// `skopos worker runner` — manage agent runners without reading the docs:
// list what is configured, add a runner + account (an interactive wizard,
// or flags for scripts), and smoke-test accounts with a real one-prompt
// run through the locked-down adapter.
func workerRunnerCmd() *cli.Command {
	return &cli.Command{
		Name:  "runner",
		Usage: "Manage agent runners: list, add (wizard or flags), test",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "Show runners, their CLIs, lockdown status and accounts",
				Flags: []cli.Flag{envFileFlag()},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					cfg, _, err := loadWorkerForTools(cmd)
					if err != nil {
						return err
					}
					printRunnerList(ctx, os.Stdout, cfg)
					return nil
				},
			},
			{
				Name:    "add",
				Usage:   "Add a runner and its account — interactive wizard, or non-interactive with --kind",
				MaxArgs: 0,
				Flags: []cli.Flag{
					envFileFlag(),
					&cli.StringFlag{Name: "kind", Usage: "claude | opencode | command (omit for the wizard)"},
					&cli.StringFlag{Name: "name", Usage: "Runner name (accounts and --run-stage use it), e.g. opencode-glm"},
					&cli.StringFlag{Name: "model", Usage: "Model for the account (claude alias, or opencode provider/model)"},
					&cli.StringFlag{Name: "account", Usage: "Account name (default: the runner name)"},
					&cli.IntFlag{Name: "priority", Usage: "Account priority (lower is preferred)"},
					&cli.StringSliceFlag{Name: "env", Usage: "Account env KEY=VALUE (repeatable; use ${VAR} to reference the env file)"},
					&cli.StringFlag{Name: "opencode-config", Usage: "opencode: base config file with providers/models"},
					&cli.StringFlag{Name: "command", Usage: "command: argv template, e.g. \"aider --yes --message {prompt}\""},
					&cli.BoolFlag{Name: "probe", Usage: "Run a one-prompt smoke test after writing"},
				},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					cfgPath := configFileFor(cmd)
					envPath := defaultWorkerEnvFile(cfgPath, cmd.GetString("env-file"))
					if envPath == "" {
						envPath = workerEnvFilePath(cfgPath)
					}
					if cmd.GetString("kind") == "" {
						if !stdinIsTerminal() {
							return fmt.Errorf("no terminal for the wizard — pass --kind (claude, opencode, command) and --name/--model")
						}
						return runRunnerWizard(ctx, bufio.NewReader(os.Stdin), os.Stdout, cfgPath, envPath, wizardEnv{})
					}
					in := runnerInput{
						Kind: cmd.GetString("kind"), Name: cmd.GetString("name"), Model: cmd.GetString("model"),
						Account: cmd.GetString("account"), Priority: cmd.GetInt("priority"),
						OpencodeConfig: cmd.GetString("opencode-config"), Env: map[string]string{},
					}
					if c := cmd.GetString("command"); c != "" {
						in.Command = strings.Fields(c)
					}
					for _, kv := range cmd.GetStringSlice("env") {
						k, v, ok := strings.Cut(kv, "=")
						if !ok {
							return fmt.Errorf("--env %q: want KEY=VALUE", kv)
						}
						in.Env[strings.TrimSpace(k)] = v
					}
					if err := addRunner(os.Stdout, cfgPath, in); err != nil {
						return err
					}
					if cmd.GetBool("probe") {
						return probeAndPrint(ctx, os.Stdout, cfgPath, envPath, accountNameOf(in))
					}
					return nil
				},
			},
			{
				Name:    "test",
				Usage:   "Smoke-test accounts with a real one-prompt run (all accounts, or one runner's)",
				MaxArgs: 1,
				Flags: []cli.Flag{
					envFileFlag(),
					&cli.StringFlag{Name: "account", Usage: "Test only this account"},
					&cli.StringFlag{Name: "timeout", DefaultValue: "5m", Usage: "Per-account timeout, e.g. 90s, 10m (local models are slow)"},
				},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					cfg, _, err := loadWorkerForTools(cmd)
					if err != nil {
						return err
					}
					runner := ""
					if args := cmd.GetArgs(); len(args) > 0 {
						runner = args[0]
					}
					timeout, err := time.ParseDuration(cmd.GetString("timeout"))
					if err != nil {
						return fmt.Errorf("--timeout: %v", err)
					}
					return testAccounts(ctx, os.Stdout, cfg, runner, cmd.GetString("account"), timeout)
				},
			},
		},
	}
}

func envFileFlag() cli.Flag {
	return &cli.StringFlag{Name: "env-file", Usage: "Env file (default: skopos-worker.env next to the config)", EnvVars: []string{"SKOPOS_WORKER_ENV"}}
}

// stdinIsTerminal: a real TTY. (A ModeCharDevice check is not enough —
// /dev/null is a character device, and `worker init </dev/null` then ran
// the wizard on empty answers.)
func stdinIsTerminal() bool {
	fd := os.Stdin.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// loadWorkerForTools loads the env file and the whole [worker] section the
// way the daemon does, for list/test.
func loadWorkerForTools(cmd *cli.Command) (*worker.Config, string, error) {
	cfgPath := configFileFor(cmd)
	if _, _, err := loadWorkerEnvFile(defaultWorkerEnvFile(cfgPath, cmd.GetString("env-file"))); err != nil {
		return nil, "", err
	}
	cfg, err := decodeWorkerConfig(cfgPath)
	return cfg, cfgPath, err
}

func decodeWorkerConfig(cfgPath string) (*worker.Config, error) {
	var file struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.DecodeFile(cfgPath, &file); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", cfgPath, err)
	}
	if file.Worker == nil {
		return nil, fmt.Errorf("%s has no [worker] section — run 'skopos worker init' first", cfgPath)
	}
	home, _ := os.UserHomeDir()
	if err := file.Worker.Default(home); err != nil {
		return nil, err
	}
	return file.Worker, nil
}

// --- list ---

func printRunnerList(ctx context.Context, out io.Writer, cfg *worker.Config) {
	pf := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f, a...) }
	offered := map[string]bool{}
	for _, r := range cfg.Runners {
		offered[r] = true
	}
	names := map[string]bool{}
	for _, n := range cfg.UsedRunners() {
		names[n] = true
	}
	for n := range cfg.RunnerSpecs {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		r, err := cfg.RunnerFor(name)
		if err != nil {
			pf("%s\n  ERROR: %v\n", name, err)
			continue
		}
		bin := cfg.RunnerBin(name)
		installed := "not found on PATH"
		if v := cliVersion(ctx, bin); v != "" {
			installed = v
		}
		lock := "verified lockdown"
		if !r.Verified() {
			lock = "UNVERIFIED — runs only on wrapped stages"
		}
		offer := "offered in claims"
		if !offered[name] {
			offer = "NOT offered (not in worker.runners)"
		}
		pf("%s\n  cli:      %s (%s)\n  lockdown: %s\n  claims:   %s\n", name, bin, installed, lock, offer)
		any := false
		for _, a := range cfg.Accounts {
			ar := a.Runner
			if ar == "" {
				ar = "claude"
			}
			if ar != name {
				continue
			}
			any = true
			model := a.Model
			if model == "" {
				model = "(CLI default)"
			}
			pf("  account:  %s  model=%s  priority=%d%s\n", a.Name, model, a.Priority, envSummary(a.Env))
		}
		if !any {
			pf("  account:  none — nothing can run on this runner\n")
		}
	}
	pf("\nPin a stage to a runner on the server: skopos serve --run-stage <workspace>:<stage>=<runner>\n")
}

func envSummary(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "  env=" + strings.Join(keys, ",")
}

// cliVersion is the CLI's first --version line ("" when not runnable).
func cliVersion(ctx context.Context, bin string) string {
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "--version").CombinedOutput()
	if err != nil && len(out) == 0 {
		return "installed"
	}
	return strings.TrimSpace(firstOutputLine(out))
}

// --- test ---

func testAccounts(ctx context.Context, out io.Writer, cfg *worker.Config, runner, account string, timeout time.Duration) error {
	pf := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f, a...) }
	failed, ran := 0, 0
	for _, a := range cfg.Accounts {
		ar := a.Runner
		if ar == "" {
			ar = "claude"
		}
		if (runner != "" && ar != runner) || (account != "" && a.Name != account) {
			continue
		}
		ran++
		pf("%s (runner %s, model %s) … ", a.Name, ar, orDash(a.Model))
		// An env value that resolved empty means the account is not set
		// up as intended — the CLI would quietly fall back to its own
		// login and the probe would test the wrong credentials.
		if empty := emptyEnv(a.Env); len(empty) > 0 {
			failed++
			pf("FAIL\n  env %s resolved empty — set the ${VAR} it references in the env file (otherwise the CLI falls back to its own login)\n", strings.Join(empty, ", "))
			continue
		}
		res := cfg.ProbeAccount(ctx, a, timeout)
		if res.Err != nil {
			failed++
			pf("FAIL after %s\n  %v\n", res.Duration.Round(time.Second), res.Err)
			continue
		}
		pf("OK in %s\n", res.Duration.Round(time.Second))
	}
	if ran == 0 {
		return fmt.Errorf("no account matches (runner %q, account %q)", runner, account)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d account(s) failed", failed, ran)
	}
	return nil
}

func emptyEnv(env map[string]string) []string {
	var out []string
	for k, v := range env {
		if v == "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func probeAndPrint(ctx context.Context, out io.Writer, cfgPath, envPath, account string) error {
	if _, _, err := loadWorkerEnvFile(envPathIfExists(envPath)); err != nil {
		return err
	}
	cfg, err := decodeWorkerConfig(cfgPath)
	if err != nil {
		return err
	}
	return testAccounts(ctx, out, cfg, "", account, 5*time.Minute)
}

func envPathIfExists(p string) string {
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// --- add: config edits (pure, validated) ---

type runnerInput struct {
	Kind           string
	Name           string
	Model          string
	Account        string
	Priority       int
	Env            map[string]string
	OpencodeConfig string
	Command        []string
}

func accountNameOf(in runnerInput) string {
	if in.Account != "" {
		return in.Account
	}
	return in.Name
}

var reRunnerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// addRunner edits the config file: the runner_config block (when the name
// needs one), the account, and an explicit `runners` list. The result is
// validated before it is written — a failed edit leaves the file untouched.
func addRunner(out io.Writer, cfgPath string, in runnerInput) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w (run 'skopos worker init' first)", cfgPath, err)
	}
	updated, err := addRunnerToConfig(string(raw), in)
	if err != nil {
		return err
	}
	if err := validateWorkerTOML(updated); err != nil {
		return fmt.Errorf("the edited config would be invalid (%s left unchanged): %w", cfgPath, err)
	}
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Added runner %s (account %s) to %s\n", in.Name, accountNameOf(in), cfgPath)
	return nil
}

func validateWorkerTOML(content string) error {
	var file struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(content, &file); err != nil {
		return err
	}
	if file.Worker == nil {
		return fmt.Errorf("no [worker] section")
	}
	home, _ := os.UserHomeDir()
	return file.Worker.Default(home)
}

// addRunnerToConfig returns content with the runner and account added.
func addRunnerToConfig(content string, in runnerInput) (string, error) {
	if !reRunnerName.MatchString(in.Name) {
		return "", fmt.Errorf("runner name %q: use letters, digits, - and _", in.Name)
	}
	switch in.Kind {
	case "claude", "opencode":
	case "command":
		if len(in.Command) == 0 {
			return "", fmt.Errorf("kind command needs a command template")
		}
	default:
		return "", fmt.Errorf("kind %q: claude, opencode or command", in.Kind)
	}
	if !hasTOMLSection(content, "worker") {
		return "", fmt.Errorf("no [worker] section — run 'skopos worker init' first")
	}
	var cur struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(content, &cur); err != nil {
		return "", err
	}
	acct := accountNameOf(in)
	nextPriority := 1
	for _, a := range cur.Worker.Accounts {
		if a.Name == acct {
			return "", fmt.Errorf("an account named %q already exists — pick another --account name", acct)
		}
		if a.Runner == in.Name && a.Priority >= nextPriority {
			nextPriority = a.Priority + 1
		}
	}
	if in.Priority == 0 {
		in.Priority = nextPriority // after the runner's existing accounts
	}

	var b strings.Builder
	// A runner_config block unless the name IS a built-in kind with no
	// settings (plain "claude" / "opencode").
	needsSpec := in.Name != in.Kind || in.OpencodeConfig != "" || len(in.Command) > 0
	if spec := cur.Worker.RunnerSpecs[in.Name]; spec != nil {
		kind := spec.Kind
		if kind == "" {
			kind = in.Name
		}
		if kind != in.Kind {
			return "", fmt.Errorf("runner %q already exists with kind %s", in.Name, kind)
		}
		needsSpec = false // reuse it: this adds another account
	}
	if needsSpec {
		fmt.Fprintf(&b, "\n[worker.runner_config.%s]\nkind = %s\n", in.Name, tomlStr(in.Kind))
		if in.OpencodeConfig != "" {
			fmt.Fprintf(&b, "config = %s\n", tomlStr(in.OpencodeConfig))
		}
		if len(in.Command) > 0 {
			fmt.Fprintf(&b, "command = %s\n", tomlStrList(in.Command))
		}
	}
	fmt.Fprintf(&b, "\n[[worker.accounts]]\nname = %s\nrunner = %s\n", tomlStr(acct), tomlStr(in.Name))
	if in.Model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlStr(in.Model))
	}
	if in.Priority != 0 {
		fmt.Fprintf(&b, "priority = %d\n", in.Priority)
	}
	if len(in.Env) > 0 {
		keys := make([]string, 0, len(in.Env))
		for k := range in.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+" = "+tomlStr(in.Env[k]))
		}
		fmt.Fprintf(&b, "env = { %s }\n", strings.Join(parts, ", "))
	}

	out := content
	// An explicit runners list must include the new runner (empty = the
	// default: every runner the accounts use).
	if len(cur.Worker.Runners) > 0 && !contains(cur.Worker.Runners, in.Name) {
		var err error
		out, err = appendToRunnersLine(out, in.Name)
		if err != nil {
			return "", err
		}
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + b.String(), nil
}

// appendToRunnersLine adds name to the single-line `runners = [...]` in the
// [worker] table.
func appendToRunnersLine(content, name string) (string, error) {
	lines := strings.Split(content, "\n")
	inWorker := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			inWorker = t == "[worker]"
			continue
		}
		if !inWorker || !strings.HasPrefix(t, "runners") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok || strings.TrimSpace(k) != "runners" {
			continue
		}
		v = strings.TrimSpace(v)
		if i := strings.Index(v, "#"); i >= 0 && strings.LastIndex(v[:i], "]") >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		var list []string
		if _, err := toml.Decode("x = "+v, &struct {
			X *[]string `toml:"x"`
		}{&list}); err != nil || !strings.HasSuffix(v, "]") {
			return "", fmt.Errorf("could not update the multi-line `runners` list in [worker] — add %q to it by hand, or remove the line (the default offers every runner your accounts use)", name)
		}
		list = append(list, name)
		indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		lines[i] = indent + "runners = " + tomlStrList(list)
		return strings.Join(lines, "\n"), nil
	}
	return content, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// tomlStr is a TOML basic string (JSON escaping is valid TOML here).
func tomlStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlStrList(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = tomlStr(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// appendEnvVar adds KEY=VALUE to the env file (created 0600) unless KEY is
// already set there. Returns whether it wrote.
func appendEnvVar(path, key, value string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, l := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "export "))
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key {
			return false, nil
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	prefix := ""
	if len(raw) > 0 && !strings.HasSuffix(string(raw), "\n") {
		prefix = "\n"
	}
	if _, err := fmt.Fprintf(f, "%s%s=%s\n", prefix, key, value); err != nil {
		return false, err
	}
	return true, os.Chmod(path, 0o600)
}

// --- the wizard ---

// wizardEnv lets tests point the wizard at fake CLIs and a fake opencode
// user config.
type wizardEnv struct {
	opencodeUserConfig string // default ~/.config/opencode/opencode.json
}

var detectableCLIs = []struct{ bin, label string }{
	{"claude", "Claude Code — built-in runner, verified lockdown"},
	{"opencode", "opencode — built-in runner, verified lockdown"},
	{"codex", "OpenAI Codex CLI — use as a command runner (wrapper-only)"},
	{"gemini", "Gemini CLI — use as a command runner (wrapper-only)"},
	{"agy", "Antigravity CLI — use as a command runner (wrapper-only)"},
	{"aider", "Aider — use as a command runner (wrapper-only)"},
}

func runRunnerWizard(ctx context.Context, in *bufio.Reader, out io.Writer, cfgPath, envPath string, env wizardEnv) error {
	say := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f, a...) }
	ask := func(label, def string) string { return promptTo(in, out, label, def) }
	yes := func(label string, def bool) bool {
		d := "Y/n"
		if !def {
			d = "y/N"
		}
		a := strings.ToLower(ask(label+" ["+d+"]", ""))
		if a == "" {
			return def
		}
		return strings.HasPrefix(a, "y")
	}

	cfg, err := decodeWorkerConfigLoose(cfgPath)
	if err != nil {
		return err
	}
	say("Add an agent runner to %s\n\nAgent CLIs on this machine:\n", cfgPath)
	found := map[string]bool{}
	for _, c := range detectableCLIs {
		v := cliVersion(ctx, c.bin)
		mark := "  - "
		if v != "" {
			found[c.bin] = true
			mark = "  ✓ "
		} else {
			v = "not installed"
		}
		say("%s%-9s %s (%s)\n", mark, c.bin, c.label, v)
	}

	def := "1"
	if !found["claude"] && found["opencode"] {
		def = "2"
	}
	say("\nRunner kind:\n  1) claude    — Claude Code\n  2) opencode  — opencode, any provider/model it supports\n  3) command   — any other CLI from an argv template (unverified: runs only inside a wrapper)\n")
	var kind string
	switch ask("Choose [1-3]", def) {
	case "2", "opencode":
		kind = "opencode"
	case "3", "command":
		kind = "command"
	default:
		kind = "claude"
	}

	ri := runnerInput{Kind: kind, Env: map[string]string{}}
	var envWrites [][2]string // KEY, VALUE to append to the env file
	var notes []string

	switch kind {
	case "claude":
		say("\nModel — an alias (sonnet, opus, haiku, fable) or a full model id; empty = the CLI's default.\n")
		ri.Model = ask("Model", "")
	case "opencode":
		models := opencodeModels(ctx)
		if len(models) > 0 {
			say("\nModels opencode knows on this machine:\n")
			for i, m := range models {
				if i >= 60 {
					say("  … (%d more — type an id)\n", len(models)-60)
					break
				}
				say("  %2d) %s\n", i+1, m)
			}
		}
		for ri.Model == "" {
			a := ask("Model (number or provider/model id)", "")
			if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(models) {
				ri.Model = models[n-1]
			} else if strings.Contains(a, "/") {
				ri.Model = a
			} else {
				say("  enter a number from the list or a provider/model id\n")
			}
		}
	case "command":
		say("\nCommand template — the agent CLI and its args. Placeholders: {prompt} {dir} {model};\nwithout {prompt} the prompt goes to stdin. Example: aider --yes --message {prompt}\n")
		for len(ri.Command) == 0 {
			ri.Command = strings.Fields(ask("Command", ""))
		}
		ri.Model = ask("Model (passed as {model}; optional)", "")
	}

	ri.Name = ask("\nRunner name (accounts and --run-stage use it)", suggestRunnerName(kind, ri.Model, ri.Command))
	for !reRunnerName.MatchString(ri.Name) {
		ri.Name = ask("Runner name (letters, digits, - and _)", suggestRunnerName(kind, ri.Model, ri.Command))
	}

	// Credentials.
	switch kind {
	case "claude":
		say("\nCredentials:\n  1) this machine's Claude login (subscription or key the CLI already uses)\n  2) an API key kept in the env file\n  3) a separate login directory (a second subscription account)\n")
		switch ask("Choose [1-3]", "1") {
		case "2":
			v := ask("Env variable for the key", "ANTHROPIC_KEY_"+strings.ToUpper(strings.ReplaceAll(ri.Name, "-", "_")))
			ri.Env["ANTHROPIC_API_KEY"] = "${" + v + "}"
			envWrites = append(envWrites, [2]string{v, ""})
			notes = append(notes, fmt.Sprintf("put the API key in %s: %s=sk-ant-…", envPath, v))
		case "3":
			dir := ask("Login directory", "~/.claude-"+ri.Name)
			ri.Env["CLAUDE_CONFIG_DIR"] = dir
			notes = append(notes, fmt.Sprintf("log that account in once: CLAUDE_CONFIG_DIR=%s claude   (then /login)", dir))
		}
	case "opencode":
		provider, _, _ := strings.Cut(ri.Model, "/")
		userCfg := env.opencodeUserConfig
		if userCfg == "" {
			home, _ := os.UserHomeDir()
			userCfg = filepath.Join(home, ".config", "opencode", "opencode.json")
		}
		block, keyVar, keyVal, isCustom := customOpencodeProvider(userCfg, provider)
		if isCustom {
			base := filepath.Join(filepath.Dir(cfgPath), "opencode-providers.json")
			for _, s := range cfg.RunnerSpecs {
				if s != nil && s.Config != "" {
					base = s.Config // reuse an existing providers file
					break
				}
			}
			say("\n%q is a custom provider from your opencode config. Worker runs never read\n%s, so it needs a providers-only config.\n", provider, userCfg)
			if yes(fmt.Sprintf("Write the %q provider to %s", provider, base), true) {
				if err := mergeOpencodeProvider(base, provider, block, ri.Model, keyVar); err != nil {
					return err
				}
				say("  wrote %s\n", base)
				ri.OpencodeConfig = base
				if keyVar != "" {
					ri.Env[keyVar] = "${" + keyVar + "}"
					if keyVal != "" && yes(fmt.Sprintf("Copy its API key from your opencode config into %s (as %s)", envPath, keyVar), true) {
						envWrites = append(envWrites, [2]string{keyVar, keyVal})
					} else {
						envWrites = append(envWrites, [2]string{keyVar, ""})
						notes = append(notes, fmt.Sprintf("put the provider key in %s: %s=…", envPath, keyVar))
					}
				}
			}
		} else {
			notes = append(notes, fmt.Sprintf("provider %q must be logged in for the user the worker runs as: opencode auth login", provider))
		}
	case "command":
		notes = append(notes, "command runners only run on wrapped stages: add a worker.profiles.<name>.<stage> with a wrapper for every stage that uses this runner (see docs/deployment/worker.md)")
	}

	ri.Account = ask("Account name", ri.Name)
	pri := 1
	for _, a := range cfg.Accounts {
		if a.Runner == ri.Name && a.Priority >= pri {
			pri = a.Priority + 1
		}
	}
	if p, err := strconv.Atoi(ask("Priority (lower is preferred)", strconv.Itoa(pri))); err == nil {
		ri.Priority = p
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	updated, err := addRunnerToConfig(string(raw), ri)
	if err != nil {
		return err
	}
	say("\nThis will be added to %s:\n%s", cfgPath, strings.TrimPrefix(updated, string(raw)))
	if len(envWrites) > 0 {
		say("and to %s: %s\n", envPath, strings.Join(envKeys(envWrites), ", "))
	}
	if !yes("Write it", true) {
		say("Nothing written.\n")
		return nil
	}
	if err := validateWorkerTOML(updated); err != nil {
		return fmt.Errorf("the edited config would be invalid (nothing written): %w", err)
	}
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		return err
	}
	for _, kv := range envWrites {
		if _, err := appendEnvVar(envPath, kv[0], kv[1]); err != nil {
			return err
		}
	}
	say("Written.\n")
	for _, n := range notes {
		say("  NOTE: %s\n", n)
	}
	if yes("\nRun a smoke test now (one real prompt through the locked-down runner)", true) {
		if err := probeAndPrint(ctx, out, cfgPath, envPath, ri.Account); err != nil {
			say("The runner is configured, but the smoke test failed: %v\nFix it, then re-run: skopos worker runner test %s\n", err, ri.Name)
		}
	}
	say("\nNext:\n  - pin stages to it on the server: skopos serve --run-stage <workspace>:<stage>=%s\n  - review all runners: skopos worker runner list\n  - restart the worker to pick it up\n", ri.Name)
	return nil
}

func envKeys(kvs [][2]string) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv[0]
	}
	return out
}

// decodeWorkerConfigLoose reads [worker] without validating (the wizard
// only needs existing names; Default() would fail on half-configured
// files the wizard is about to fix).
func decodeWorkerConfigLoose(cfgPath string) (*worker.Config, error) {
	var file struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.DecodeFile(cfgPath, &file); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s not found — run 'skopos worker init' first", cfgPath)
		}
		return nil, fmt.Errorf("parsing %s: %w", cfgPath, err)
	}
	if file.Worker == nil {
		return nil, fmt.Errorf("%s has no [worker] section — run 'skopos worker init' first", cfgPath)
	}
	return file.Worker, nil
}

func promptTo(in *bufio.Reader, out io.Writer, label, def string) string {
	if def != "" {
		_, _ = fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		_, _ = fmt.Fprintf(out, "%s: ", label)
	}
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if (err != nil && line == "") || line == "" {
		return def
	}
	return line
}

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

func suggestRunnerName(kind, model string, command []string) string {
	slug := func(s string) string {
		s = strings.Trim(reSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
		if len(s) > 30 {
			s = s[:30]
		}
		return s
	}
	switch kind {
	case "claude":
		if model == "" {
			return "claude"
		}
		return "claude-" + slug(model)
	case "opencode":
		m := model
		if i := strings.LastIndex(m, "/"); i >= 0 {
			m = m[i+1:]
		}
		return "opencode-" + slug(m)
	default:
		if len(command) > 0 {
			return slug(filepath.Base(command[0]))
		}
		return "custom"
	}
}

// opencodeModels lists `opencode models` (the user's own view — custom
// providers included); empty when the CLI is missing.
func opencodeModels(ctx context.Context) []string {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "opencode", "models").Output()
	if err != nil {
		return nil
	}
	var models []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(stripANSI(l))
		if strings.Contains(l, "/") && !strings.ContainsAny(l, " \t") {
			models = append(models, l)
		}
	}
	return models
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

// customOpencodeProvider finds provider in the user's opencode config. A
// literal apiKey is returned (to move into the env file) and replaced by
// {env:VAR}; an existing {env:X} reference is kept.
func customOpencodeProvider(userCfg, provider string) (block map[string]any, keyVar, keyVal string, ok bool) {
	raw, err := os.ReadFile(userCfg)
	if err != nil {
		return nil, "", "", false
	}
	var cfg struct {
		Provider map[string]map[string]any `json:"provider"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil, "", "", false
	}
	block, ok = cfg.Provider[provider]
	if !ok {
		return nil, "", "", false
	}
	opts, _ := block["options"].(map[string]any)
	if key, _ := opts["apiKey"].(string); key != "" {
		if strings.HasPrefix(key, "{env:") && strings.HasSuffix(key, "}") {
			keyVar = strings.TrimSuffix(strings.TrimPrefix(key, "{env:"), "}")
		} else {
			keyVar = strings.ToUpper(reSlug.ReplaceAllString(strings.ToLower(provider), "_")) + "_API_KEY"
			keyVal = key
			opts["apiKey"] = "{env:" + keyVar + "}"
		}
	}
	return block, keyVar, keyVal, true
}

// mergeOpencodeProvider writes provider into the providers-only base config
// (created or merged), making sure the chosen model is listed.
func mergeOpencodeProvider(path, provider string, block map[string]any, model, keyVar string) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
	}
	providers, _ := doc["provider"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	models, _ := block["models"].(map[string]any)
	if models == nil {
		models = map[string]any{}
	}
	if _, id, ok := strings.Cut(model, "/"); ok {
		if _, has := models[id]; !has {
			models[id] = map[string]any{"name": id}
		}
	}
	block["models"] = models
	providers[provider] = block
	doc["provider"] = providers
	if _, ok := doc["$schema"]; !ok {
		doc["$schema"] = "https://opencode.ai/config.json"
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
