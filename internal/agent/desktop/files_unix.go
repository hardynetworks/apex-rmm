//go:build !windows

package desktop

import (
	"os"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
)

// consoleOwner returns the uid/gid of the signed-in desktop user: the owner of /dev/console on
// macOS, or of the X authority file on Linux.
func consoleOwner() (uid, gid int, ok bool) {
	path := os.Getenv("XAUTHORITY")
	if runtime.GOOS == "darwin" {
		path = "/dev/console"
	}
	if path == "" {
		return 0, 0, false
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid == 0 {
		return 0, 0, false
	}
	return int(sys.Uid), int(sys.Gid), true
}

func consoleUserHome() string {
	uid, _, ok := consoleOwner()
	if !ok {
		return ""
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// giveToUser makes an uploaded file belong to the signed-in user rather than root.
func giveToUser(path string) {
	if os.Geteuid() != 0 {
		return
	}
	if uid, gid, ok := consoleOwner(); ok {
		_ = os.Chown(path, uid, gid)
	}
}

func roots() []string { return []string{"/"} }
