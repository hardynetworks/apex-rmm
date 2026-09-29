//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

func isAdmin() bool { return os.Geteuid() == 0 }

func restrictFile(string) error { return nil }

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func postInstall() {}

func hideWindow(*exec.Cmd) {}
