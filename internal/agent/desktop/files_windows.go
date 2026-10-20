//go:build windows

package desktop

import (
	"golang.org/x/sys/windows"
)

// consoleUserHome returns the profile folder of the user signed in to this session (the helper
// runs as SYSTEM, whose own profile is of no use to the technician).
func consoleUserHome() string {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return ""
	}
	var tok windows.Token
	if err := windows.WTSQueryUserToken(session, &tok); err != nil {
		return ""
	}
	defer tok.Close()
	dir, err := tok.GetUserProfileDirectory()
	if err != nil {
		return ""
	}
	return dir
}

// giveToUser: files created in the user's profile inherit its permissions, nothing to do.
func giveToUser(string) {}

func roots() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return []string{`C:\`}
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<i) != 0 {
			out = append(out, string(rune('A'+i))+`:\`)
		}
	}
	return out
}
