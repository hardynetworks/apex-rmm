//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so timeouts kill the whole tree.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}
