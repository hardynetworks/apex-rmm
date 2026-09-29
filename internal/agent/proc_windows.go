//go:build windows

package agent

import (
	"fmt"
	"os/exec"
)

// setProcGroup kills the whole process tree on timeout via taskkill /T.
func setProcGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			k := exec.Command("taskkill.exe", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid))
			hideWindow(k)
			_ = k.Run()
		}
		return nil
	}
}
