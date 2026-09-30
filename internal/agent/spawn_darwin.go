//go:build darwin

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// spawnDesktopHelper starts the capture helper inside the logged-in user's GUI
// bootstrap namespace (required for screen capture and event posting).
func spawnDesktopHelper(d proto.DesktopStart) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var st syscall.Stat_t
	uid := uint32(0)
	if err := syscall.Stat("/dev/console", &st); err == nil {
		uid = st.Uid
	}
	args := []string{"desktop", "--url", d.URL, "--session", d.SessionID, "--token", d.Token}
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("/bin/launchctl", append([]string{"asuser", fmt.Sprint(uid), exe}, args...)...)
	} else {
		cmd = exec.Command(exe, args...)
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
