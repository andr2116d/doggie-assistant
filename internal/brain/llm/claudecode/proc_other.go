//go:build !unix

package claudecode

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func (pr *proc) kill() {
	if p := pr.cmd.Process; p != nil && pr.alive() {
		p.Kill()
	}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
