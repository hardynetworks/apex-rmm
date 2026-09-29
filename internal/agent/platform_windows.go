//go:build windows

package agent

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func isAdmin() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(&windows.SECURITY_NT_AUTHORITY, 2,
		windows.SECURITY_BUILTIN_DOMAIN_RID, windows.DOMAIN_ALIAS_RID_ADMINS, 0, 0, 0, 0, 0, 0, &sid)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)
	member, err := windows.Token(0).IsMember(sid)
	return err == nil && member
}

// restrictFile limits the config to SYSTEM and Administrators.
func restrictFile(p string) error {
	cmd := exec.Command("icacls", p, "/inheritance:r", "/grant:r", "*S-1-5-18:F", "*S-1-5-32-544:F")
	hideWindow(cmd)
	return cmd.Run()
}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS, HideWindow: true}
}

func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// postInstall allows services to generate Ctrl+Alt+Del (SendSAS) for remote control.
func postInstall() {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("SoftwareSASGeneration"); err != nil || v == 0 {
		_ = k.SetDWordValue("SoftwareSASGeneration", 1)
	}
}
