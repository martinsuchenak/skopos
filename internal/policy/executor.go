package policy

import (
	"fmt"
	"strings"
)

// How a stage is contained (tmp/central-config — design.md, phase 1c). A
// worker reports, per workspace, stage and runner, the executor a run would
// get; a rule's `executor` field says what it must be.
//
// Executor ids:
//
//	local                    nothing around the agent CLI but its own lockdown
//	host:<name>              the worker says its whole host is a container
//	wrapper:<name>:<hash>    a wrapper around the run (the hash covers its argv)
//	sandbox:<hash>           Claude Code's own sandbox, on through stage settings
//	runner:<kind>            a runner whose verified lockdown is a sandbox (codex)
//	none:<why>               the worker cannot run this stage with this runner:
//	                         the wrapper the workspace's config asks for is not
//	                         usable on it (none:wrapper), or the stage is given
//	                         capabilities and nothing contains it there
//	                         (none:uncontained), or the workspace's stage
//	                         blocks cannot be used on it (none:config). No
//	                         run is handed out for it.
//
// A rule asks for one of two properties, or names an executor:
//
//	isolated     the run cannot reach other workspaces' files, the worker's
//	             state or other accounts: a wrapper, or a runner sandbox
//	contained    the run cannot reach beyond the host's container: any of
//	             those, or host:<name>
//	<glob>       that executor exactly, e.g. wrapper:opencode-bwrap:*
//
// The two differ on purpose: inside a container that is the whole host, a
// run still sees every other clone on it. A rule that is there to keep one
// repository's runs away from another's needs `isolated`.
//
// sandbox:<hash> meets neither: Claude Code's sandbox limits what the
// agent's commands write and reach over the network, but by default leaves
// the rest of the worker readable, and the worker's own setup and check
// commands run outside it. A rule that accepts it names it (sandbox:*).

// Executor requirements a rule may name besides a glob.
const (
	ExecutorIsolated  = "isolated"
	ExecutorContained = "contained"
)

// Executor classes.
const (
	ExecutorLocal   = "local"
	prefixHost      = "host:"
	prefixWrapper   = "wrapper:"
	prefixSandbox   = "sandbox:"
	prefixRunnerBox = "runner:"
	prefixNone      = "none:"
)

// Reasons a worker gives for an executor it cannot offer.
const (
	// NoneWrapper: the workspace's config wraps this runner's runs of the
	// stage in a wrapper the worker cannot use (not defined, its check
	// fails there, or it needs a variable the worker does not declare).
	NoneWrapper = "wrapper"
	// NoneUncontained: the stage is given capabilities (tools, env,
	// settings) and nothing contains its runs on the worker.
	NoneUncontained = "uncontained"
	// NoneConfig: the workspace's stage blocks on the server cannot be
	// used on the worker at all (they name a variable it does not declare,
	// or hold something it refuses).
	NoneConfig = "config"
)

// NoExecutor is what a worker reports where it cannot run a stage with a
// runner at all; why is one of the None reasons.
func NoExecutor(why string) string { return prefixNone + why }

// Unavailable reports an executor id that says the worker cannot run there.
func Unavailable(executor string) bool { return strings.HasPrefix(executor, prefixNone) }

// UnavailableReason says in words why an unavailable executor is one.
func UnavailableReason(executor string) string {
	switch strings.TrimPrefix(executor, prefixNone) {
	case NoneWrapper:
		return "the wrapper the workspace's config asks for is not usable there"
	case NoneUncontained:
		return "the stage is given capabilities and nothing contains its runs there"
	case NoneConfig:
		return "the workspace's stage blocks cannot be used there"
	}
	return "it reports that it cannot run there (" + executor + ")"
}

// HostExecutor is the executor of a run on a host the worker declares to
// be a container.
func HostExecutor(name string) string { return prefixHost + name }

// RunnerExecutor is the executor of a runner whose own verified lockdown
// is a sandbox.
func RunnerExecutor(kind string) string { return prefixRunnerBox + kind }

// WrapperExecutor is the executor of a named wrapper with this argv hash.
func WrapperExecutor(name, hash string) string { return prefixWrapper + name + ":" + hash }

// SandboxExecutor is the executor of the runner's sandbox turned on through
// stage settings.
func SandboxExecutor(hash string) string { return prefixSandbox + hash }

// Isolated reports whether an executor keeps a run away from everything
// else on the worker.
func Isolated(executor string) bool {
	return strings.HasPrefix(executor, prefixWrapper) || strings.HasPrefix(executor, prefixRunnerBox)
}

// Contained reports whether an executor keeps a run inside something: an
// isolated one, or a host that is itself a container.
func Contained(executor string) bool {
	return Isolated(executor) || strings.HasPrefix(executor, prefixHost)
}

// ExecutorSatisfies reports whether an executor meets a rule's requirement.
// An empty requirement accepts any executor, local and unreported included;
// any other requirement is not met by an unreported executor (""). An
// executor the worker cannot offer (none:…) meets nothing.
func ExecutorSatisfies(requirement, executor string) bool {
	if Unavailable(executor) {
		return false
	}
	switch requirement {
	case "":
		return true
	case ExecutorIsolated:
		return Isolated(executor)
	case ExecutorContained:
		return Contained(executor)
	}
	return executor != "" && glob(requirement, executor)
}

// KnownExecutor reports whether id is an executor id of a known class:
// local, or a class prefix with something after it.
func KnownExecutor(id string) bool {
	if id == ExecutorLocal {
		return true
	}
	for _, p := range []string{prefixHost, prefixWrapper, prefixSandbox, prefixRunnerBox, prefixNone} {
		if strings.HasPrefix(id, p) && len(id) > len(p) {
			return true
		}
	}
	return false
}

// validExecutorRequirement checks what a rule may ask for.
func validExecutorRequirement(req string) error {
	switch req {
	case "", ExecutorIsolated, ExecutorContained:
		return nil
	}
	for _, p := range []string{ExecutorLocal, prefixHost, prefixWrapper, prefixSandbox, prefixRunnerBox} {
		if req == p || strings.HasPrefix(req, p) {
			return nil
		}
	}
	return fmt.Errorf("executor %q: isolated, contained, or an executor to match (local, host:<name>, wrapper:<name>:*, sandbox:*, runner:<kind>)", req)
}

// ExecutorFact is the executor a worker's runs get in one workspace: for a
// stage and a runner, or for all of either when left empty. The most
// specific entry wins: stage and runner, then stage, then runner, then
// neither.
type ExecutorFact struct {
	Stage    string `json:"stage,omitempty"`
	Runner   string `json:"runner,omitempty"`
	Executor string `json:"executor"`
}

// ResolveExecutor finds the executor a run of stage on runner gets, from a
// worker's facts for the run's workspace ("" when it reports none that
// applies).
func ResolveExecutor(facts []ExecutorFact, stage, runner string) string {
	best, bestRank := "", -1
	for _, f := range facts {
		if (f.Stage != "" && f.Stage != stage) || (f.Runner != "" && f.Runner != runner) {
			continue
		}
		rank := 0
		if f.Runner != "" {
			rank = 1
		}
		if f.Stage != "" {
			rank += 2
		}
		if rank > bestRank {
			best, bestRank = f.Executor, rank
		}
	}
	return best
}
