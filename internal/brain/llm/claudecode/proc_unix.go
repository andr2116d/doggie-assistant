//go:build unix

package claudecode

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts claude in its own process group, so kill reaches
// the processes it starts too.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// kill stops the process group at once. A process already reaped is left
// alone: its PID, and so its group ID, may belong to someone else by now.
// (A tiny window remains between the reap and exited closing.)
func (pr *proc) kill() {
	if p := pr.cmd.Process; p != nil && pr.alive() {
		syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}
