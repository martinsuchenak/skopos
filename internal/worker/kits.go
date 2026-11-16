package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Kits on the worker (wsconfig/kits.go): a stage block that names kits gets
// their tools, and Claude Code's sandbox with their hosts and cache folders,
// resolved for this host. The sandbox they turn on refuses any host outside
// the allowlist and any command outside the sandbox, and keeps the host's
// credential files out of reach of the commands it runs.

// kitParts is what a list of kits adds to a stage on this host.
type kitParts struct {
	Tools    []string
	Commands []string
	Hosts    []string
	// Write: the cache folders, absolute.
	Write []string
	// Deny: credential files and folders on this host the sandboxed
	// commands may not read.
	Deny []string
	// problem: a folder that could not be worked out ("go env" failed).
	problem string
	at      time.Time
}

// kitCache holds resolved kit lists by their names, for this host; a list
// that could not be resolved is tried again after kitRetry.
var kitCache struct {
	mu    sync.Mutex
	parts map[string]kitParts
}

const kitRetry = time.Minute

// credentialPaths are files and folders under the home folder that hold
// credentials: the sandboxed commands of a kit may not read them.
var credentialPaths = []string{
	".ssh", ".gnupg", ".config/skopos", ".claude", ".claude.json", ".codex",
	".config/opencode", ".local/share/opencode", ".gemini", ".config/gh",
	".git-credentials", ".netrc", ".docker", ".aws", ".azure",
	".config/gcloud", ".kube", ".npmrc", ".pypirc", ".composer/auth.json",
	".config/composer/auth.json",
}

// kitParts resolves the named kits for this host (cached), with this
// worker's own secret files denied (SetSecretFiles).
func (c *Config) kitParts(names []string) kitParts {
	if len(names) == 0 {
		return kitParts{}
	}
	key := strings.Join(names, ",")
	kitCache.mu.Lock()
	p, ok := kitCache.parts[key]
	if !ok || (p.problem != "" && time.Since(p.at) >= kitRetry) {
		p = resolveKits(names)
		if kitCache.parts == nil {
			kitCache.parts = map[string]kitParts{}
		}
		kitCache.parts[key] = p
	}
	kitCache.mu.Unlock()
	p.Deny = slices.Clone(p.Deny)
	for _, path := range c.ownDeny {
		if !slices.Contains(p.Deny, path) {
			p.Deny = append(p.Deny, path)
		}
	}
	return p
}

func resolveKits(names []string) kitParts {
	p := kitParts{at: time.Now()}
	add := func(list *[]string, items ...string) {
		for _, it := range items {
			if it != "" && !slices.Contains(*list, it) {
				*list = append(*list, it)
			}
		}
	}
	home, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	goEnv := map[string]string{}
	for _, n := range names {
		k, ok := wsconfig.KitByName(n)
		if !ok {
			continue // the server and NormalizeStage refuse an unknown kit
		}
		add(&p.Tools, k.Tools...)
		add(&p.Commands, k.Commands...)
		add(&p.Hosts, k.Hosts...)
		for _, w := range k.Write {
			dir, err := kitPath(w, home, cache, goEnv)
			if err != nil {
				if p.problem == "" {
					p.problem = fmt.Sprintf("kit %s: %v", k.Name, err)
				}
				continue
			}
			add(&p.Write, dir)
		}
	}
	if home != "" {
		for _, rel := range credentialPaths {
			path := filepath.Join(home, rel)
			// Only what exists: a rule for a missing path could stop the
			// sandbox from starting.
			if _, err := os.Lstat(path); err == nil {
				add(&p.Deny, path)
			}
		}
	}
	return p
}

// kitPath resolves one of a kit's write folders: ~, {cache}, {gopath},
// {gocache}.
func kitPath(w, home, cache string, goEnv map[string]string) (string, error) {
	goVar := func(name string) (string, error) {
		if v, ok := goEnv[name]; ok {
			return v, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "go", "env", name).Output()
		if err != nil {
			return "", fmt.Errorf("`go env %s` failed (is go on the PATH?): %v", name, err)
		}
		v := strings.TrimSpace(string(out))
		if name == "GOPATH" {
			v, _, _ = strings.Cut(v, string(os.PathListSeparator))
		}
		if v == "" || v == "off" {
			return "", fmt.Errorf("`go env %s` is empty", name)
		}
		goEnv[name] = v
		return v, nil
	}
	var err error
	switch {
	case strings.HasPrefix(w, "~/"):
		if home == "" {
			return "", fmt.Errorf("no home folder for %s", w)
		}
		w = filepath.Join(home, w[2:])
	case strings.HasPrefix(w, "{cache}"):
		if cache == "" {
			return "", fmt.Errorf("no cache folder for %s", w)
		}
		w = cache + strings.TrimPrefix(w, "{cache}")
	case strings.HasPrefix(w, "{gopath}"):
		var gp string
		if gp, err = goVar("GOPATH"); err != nil {
			return "", err
		}
		w = gp + strings.TrimPrefix(w, "{gopath}")
	case strings.HasPrefix(w, "{gocache}"):
		var gc string
		if gc, err = goVar("GOCACHE"); err != nil {
			return "", err
		}
		w = gc + strings.TrimPrefix(w, "{gocache}")
	}
	if !filepath.IsAbs(w) {
		return "", fmt.Errorf("%s is not an absolute path", w)
	}
	return filepath.Clean(w), nil
}

// withKits lays what kits add over a stage's settings: the sandbox on, with
// no way out of it, the kits' hosts as the only ones reachable, their cache
// folders writable and this host's credential files unreadable. What the
// settings already say is kept, and lists are added to.
func withKits(settings string, k kitParts) string {
	top := map[string]any{}
	if strings.TrimSpace(settings) != "" {
		_ = json.Unmarshal([]byte(settings), &top)
	}
	sb, _ := top["sandbox"].(map[string]any)
	if sb == nil {
		sb = map[string]any{}
	}
	sb["enabled"] = true
	setDefault(sb, "failIfUnavailable", true)
	setDefault(sb, "allowUnsandboxedCommands", false)
	fs := subObject(sb, "filesystem")
	addStrings(fs, "allowWrite", k.Write)
	addStrings(fs, "denyRead", k.Deny)
	net := subObject(sb, "network")
	setDefault(net, "strictAllowlist", true)
	addStrings(net, "allowedDomains", k.Hosts)
	top["sandbox"] = sb
	buf, _ := json.Marshal(top)
	return string(buf)
}

// withNested turns the sandbox's nested mode on in settings that enable the
// sandbox (worker.sandbox_nested).
func withNested(settings string) string {
	top := map[string]any{}
	if json.Unmarshal([]byte(settings), &top) != nil {
		return settings
	}
	sb, _ := top["sandbox"].(map[string]any)
	if sb == nil {
		return settings
	}
	sb["enableWeakerNestedSandbox"] = true
	buf, _ := json.Marshal(top)
	return string(buf)
}

func setDefault(m map[string]any, key string, v any) {
	if _, ok := m[key]; !ok {
		m[key] = v
	}
}

func subObject(m map[string]any, key string) map[string]any {
	sub, _ := m[key].(map[string]any)
	if sub == nil {
		sub = map[string]any{}
		m[key] = sub
	}
	return sub
}

func addStrings(m map[string]any, key string, items []string) {
	if len(items) == 0 {
		return
	}
	var have []string
	if list, ok := m[key].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				have = append(have, s)
			}
		}
	}
	for _, it := range items {
		if !slices.Contains(have, it) {
			have = append(have, it)
		}
	}
	m[key] = have
}
