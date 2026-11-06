//go:build unix

package worker

import (
	"errors"
	"os/exec"
	"syscall"
)

// containProcess makes a command's whole process tree stoppable: it runs
// in its own process group, and a cancelled or timed-out context kills
// that group, not just the shell or CLI at its top. Without this, a setup
// script's child (php prepare_checkout.php → dothis) outlived its killed
// shell, kept the output pipe open, and the worker waited on it forever
// while its heartbeats kept the run looking busy. WaitDelay is the second
// guard: once the top process is gone (killed, or exited leaving a
// background child holding the pipe), the pipes close after it and Wait
// returns.
func containProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = processWaitDelay
}
