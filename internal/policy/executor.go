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
)

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
// any other requirement is not met by an unreported executor ("").
func ExecutorSatisfies(requirement, executor string) bool {
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
	for _, p := range []string{prefixHost, prefixWrapper, prefixSandbox, prefixRunnerBox} {
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
