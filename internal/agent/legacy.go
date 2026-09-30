package agent

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/kardianos/service"
)

// The product used to be called "Hardy RMM". removeLegacyAgent stops and removes
// an old "hardy-agent" install on this machine so the two don't run side by side.
func removeLegacyAgent() {
	var exe string
	var paths []string
	switch runtime.GOOS {
	case "windows":
		pf, pd := os.Getenv("ProgramFiles"), os.Getenv("ProgramData")
		if pf == "" {
			pf = `C:\Program Files`
		}
		if pd == "" {
			pd = `C:\ProgramData`
		}
		exe = pf + `\HardyRMM\hardy-agent.exe`
		paths = []string{pf + `\HardyRMM`, pd + `\HardyRMM`}
	case "darwin":
		exe = "/usr/local/hardy-agent/hardy-agent"
		paths = []string{"/usr/local/hardy-agent", "/Library/Application Support/HardyRMM", "/Library/Logs/HardyRMM"}
	default:
		exe = "/usr/local/bin/hardy-agent"
		paths = []string{exe, "/etc/hardy-agent", "/var/log/hardy-agent"}
	}
	found := false
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			found = true
		}
	}
	s, err := service.New(&program{}, &service.Config{Name: "hardy-agent", Executable: exe})
	if err == nil {
		if _, err := s.Status(); err == nil {
			found = true
			_ = s.Stop()
			time.Sleep(2 * time.Second)
			_ = s.Uninstall()
		}
	}
	if !found {
		return
	}
	for _, p := range paths {
		_ = os.RemoveAll(p)
	}
	fmt.Println("Removed the old Hardy RMM agent.")
}
