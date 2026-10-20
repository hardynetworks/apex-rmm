//go:build windows

package desktop

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Working out where a file dropped at a point should go:
//   - on the desktop → the user's Desktop folder
//   - on an Explorer window → the folder it shows (asked from Explorer through Shell.Application,
//     in a short PowerShell run as the signed-in user, since the helper itself is SYSTEM)
//   - on any other program → the user's Downloads folder; the viewer then also puts the files on
//     the clipboard so Ctrl+V pastes them into that program.
var (
	pWindowFromPoint  = user32.NewProc("WindowFromPoint")
	pGetAncestor      = user32.NewProc("GetAncestor")
	pGetClassName     = user32.NewProc("GetClassNameW")
	pGetWindowText    = user32.NewProc("GetWindowTextW")
	pGetForegroundWnd = user32.NewProc("GetForegroundWindow")
)

func windowClass(h uintptr) string {
	var b [256]uint16
	n, _, _ := pGetClassName.Call(h, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	return windows.UTF16ToString(b[:n])
}

func windowTitle(h uintptr) string {
	var b [512]uint16
	n, _, _ := pGetWindowText.Call(h, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	return windows.UTF16ToString(b[:n])
}

func userToken() (windows.Token, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return 0, err
	}
	var tok windows.Token
	if err := windows.WTSQueryUserToken(session, &tok); err != nil {
		return 0, fmt.Errorf("no signed-in user: %w", err)
	}
	return tok, nil
}

func userFolder(id *windows.KNOWNFOLDERID) (string, error) {
	tok, err := userToken()
	if err != nil {
		return "", err
	}
	defer tok.Close()
	return tok.KnownFolderPath(id, 0)
}

func resolveDrop(gx, gy float64, foreground bool) (dropTarget, error) {
	var h uintptr
	if foreground {
		h, _, _ = pGetForegroundWnd.Call()
	} else {
		pt := uintptr(uint32(int32(gx))) | uintptr(uint32(int32(gy)))<<32 // POINT by value
		h, _, _ = pWindowFromPoint.Call(pt)
	}
	if h != 0 {
		if root, _, _ := pGetAncestor.Call(h, 2 /*GA_ROOT*/); root != 0 {
			h = root
		}
	}
	class := ""
	if h != 0 {
		class = windowClass(h)
	}
	switch class {
	case "Progman", "WorkerW":
		if p, err := userFolder(windows.FOLDERID_Desktop); err == nil {
			return dropTarget{Path: p, Label: "Desktop", Kind: "desktop"}, nil
		}
	case "CabinetWClass", "ExploreWClass":
		if p, err := explorerFolder(h, windowTitle(h)); err == nil && p != "" {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return dropTarget{Path: p, Label: filepath.Base(p), Kind: "folder"}, nil
			}
		}
	}
	p, err := userFolder(windows.FOLDERID_Downloads)
	if err != nil {
		return dropTarget{}, err
	}
	label := windowTitle(h)
	if label == "" {
		label = "the program"
	}
	return dropTarget{Path: p, Label: label, Kind: "app"}, nil
}

// explorerFolder asks the user's Explorer which folder the window shows. With tabs (Windows 11)
// every tab shares the window; the active one is the tab whose name is the window title.
func explorerFolder(hwnd uintptr, title string) (string, error) {
	script := fmt.Sprintf(`[Console]::OutputEncoding=[Text.Encoding]::UTF8
$h=[int64]%d; $t='%s'; $best=$null
foreach($w in (New-Object -ComObject Shell.Application).Windows()){
  try { if([int64]$w.HWND -eq $h){ $p=$w.Document.Folder.Self.Path; if(-not $best){$best=$p}; if($w.LocationName -eq $t){$best=$p; break} } } catch {}
}
[Console]::Out.Write($best)`, hwnd, strings.ReplaceAll(title, "'", "''"))
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[2*i], b[2*i+1] = byte(v), byte(v>>8)
	}
	sys32, _ := windows.GetSystemDirectory()
	ps := filepath.Join(sys32, `WindowsPowerShell\v1.0\powershell.exe`)
	cmd := fmt.Sprintf(`"%s" -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand %s`, ps, base64.StdEncoding.EncodeToString(b))
	out, err := runAsUser(cmd, 8*time.Second)
	return strings.TrimSpace(out), err
}

// runAsUser runs a command line as the signed-in user (hidden) and returns its output.
func runAsUser(cmdline string, timeout time.Duration) (string, error) {
	tok, err := userToken()
	if err != nil {
		return "", err
	}
	defer tok.Close()
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, tok, false); err != nil {
		return "", err
	}
	defer windows.DestroyEnvironmentBlock(env)
	var r, w windows.Handle
	sa := windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreatePipe(&r, &w, &sa, 0); err != nil {
		return "", err
	}
	defer windows.CloseHandle(r)
	_ = windows.SetHandleInformation(r, windows.HANDLE_FLAG_INHERIT, 0)
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := &windows.StartupInfo{Desktop: desktop, Flags: windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE, StdOutput: w, StdErr: w}
	si.Cb = uint32(unsafe.Sizeof(*si))
	var pi windows.ProcessInformation
	cl, _ := windows.UTF16PtrFromString(cmdline)
	err = windows.CreateProcessAsUser(tok, nil, cl, nil, nil, true, windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, env, nil, si, &pi)
	windows.CloseHandle(w)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	done := make(chan []byte, 1)
	go func() {
		var out []byte
		buf := make([]byte, 4096)
		for {
			var n uint32
			if err := windows.ReadFile(r, buf, &n, nil); err != nil || n == 0 {
				break
			}
			out = append(out, buf[:n]...)
			if len(out) > 64<<10 {
				break
			}
		}
		done <- out
	}()
	select {
	case out := <-done:
		return string(out), nil
	case <-time.After(timeout):
		_ = windows.TerminateProcess(pi.Process, 1)
		return "", errors.New("timed out")
	}
}
