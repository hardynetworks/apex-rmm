//go:build windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"github.com/hardynetworks/hardy-rmm/internal/proto"
	"golang.org/x/sys/windows"
)

// activeSession returns the console session, or the first active RDP session.
func activeSession() uint32 {
	if s := windows.WTSGetActiveConsoleSessionId(); s != 0xFFFFFFFF {
		return s
	}
	var infos *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &infos, &count); err == nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(infos)))
		list := unsafe.Slice(infos, count)
		for _, s := range list {
			if s.State == windows.WTSActive {
				return s.SessionID
			}
		}
	}
	return 1
}

// spawnDesktopHelper launches the capture helper as SYSTEM inside the active
// user session so it can see the user's desktop as well as UAC and the lock screen.
func spawnDesktopHelper(d proto.DesktopStart) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmdline := fmt.Sprintf(`"%s" desktop --url "%s" --session "%s" --token "%s"`, exe, d.URL, d.SessionID, d.Token)

	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &self); err != nil {
		return err
	}
	defer self.Close()
	if !self.IsElevated() {
		// Not running as a service (dev mode): just start it normally.
		cmd := exec.Command(exe, "desktop", "--url", d.URL, "--session", d.SessionID, "--token", d.Token)
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
	var dup windows.Token
	if err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil, windows.SecurityIdentification, windows.TokenPrimary, &dup); err != nil {
		return fmt.Errorf("duplicate token: %w", err)
	}
	defer dup.Close()
	session := activeSession()
	if err := windows.SetTokenInformation(dup, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4); err != nil {
		return fmt.Errorf("set session id: %w", err)
	}
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := &windows.StartupInfo{Desktop: desktop}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESHOWWINDOW
	si.ShowWindow = windows.SW_HIDE
	var pi windows.ProcessInformation
	cl, _ := windows.UTF16PtrFromString(cmdline)
	if err := windows.CreateProcessAsUser(dup, nil, cl, nil, nil, false, windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, nil, nil, si, &pi); err != nil {
		return fmt.Errorf("start helper in session %d: %w", session, err)
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}
