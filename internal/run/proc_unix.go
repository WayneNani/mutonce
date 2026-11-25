//go:build unix

package run

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own, so a timeout
// kills its children too.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// killedByOS reports whether the process died from a SIGKILL that mutonce
// did not send (its own kills are timeouts or exceeded limits): almost
// always the kernel's OOM killer, for example under a cgroup limit.
func killedByOS(exit *exec.ExitError) bool {
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}
