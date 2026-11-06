//go:build !unix

package worker

import "os/exec"

// containProcess: without process groups, the context kills the top
// process, and WaitDelay stops Wait from waiting on pipes a child kept.
func containProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = processWaitDelay
}
