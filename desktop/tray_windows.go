//go:build windows

package main

import (
	"fmt"
	"unsafe"
)

type trayIcon struct {
	hwnd           uintptr
	icon           uintptr
	taskbarCreated uint32
	hinted         bool
}

const (
	cmdOpen = iota + 1
	cmdChangeServer
	cmdCaptureKeys
	cmdAutostart
	cmdCheckUpdate
	cmdInstallUpdate
	cmdQuit
)

func newTray(hwnd uintptr) *trayIcon {
	t := &trayIcon{hwnd: hwnd, icon: appIcon(false)}
	r, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	t.taskbarCreated = uint32(r)
	t.add()
	return t
}

func (t *trayIcon) data() *notifyIconData {
	d := &notifyIconData{HWnd: t.hwnd, UID: 1}
	d.CbSize = uint32(unsafe.Sizeof(*d))
	return d
}

func (t *trayIcon) add() {
	d := t.data()
	d.UFlags = nifMessage | nifIcon | nifTip | nifShowTip
	d.UCallbackMessage = wmAppTray
	d.HIcon = t.icon
	copyU16(d.SzTip[:], appName)
	pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(d)))
	d.UVersion = notifyIconVersion4
	pShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(d)))
}

func (t *trayIcon) remove() {
	d := t.data()
	pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(d)))
}

// balloon shows a Windows notification from the tray icon.
func (t *trayIcon) balloon(title, body string) {
	d := t.data()
	d.UFlags = nifInfo
	copyU16(d.SzInfoTitle[:], title)
	if body == "" {
		body = " "
	}
	copyU16(d.SzInfo[:], body)
	d.DwInfoFlags = niifUser | niifLargeIcon
	d.HBalloonIcon = appIcon(true)
	pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(d)))
}

// hintOnce tells the user (once per run) that closing the window keeps Apex RMM in the tray.
func (t *trayIcon) hintOnce() {
	if t.hinted {
		return
	}
	t.hinted = true
	a.notifyURL = ""
	t.balloon(appName+" is still running", "Alerts keep arriving here. Right-click the tray icon to quit.")
}

func (t *trayIcon) onEvent(ev uint32) {
	switch ev {
	case ninSelect, wmLButtonDblClk:
		a.showMain()
	case wmContextMenu, wmRButtonUp:
		t.menu()
	case ninBalloonUserClick:
		switch {
		case a.notifyURL == "#update":
			if a.update != nil {
				a.installUpdate()
			}
		case a.notifyURL != "" && a.cfg.Server != "":
			a.showMain()
			if a.main != nil {
				a.main.navigate(a.cfg.Server + a.notifyURL)
			}
		default:
			a.showMain()
		}
	}
}

func (t *trayIcon) menu() {
	m, _, _ := pCreatePopupMenu.Call()
	defer pDestroyMenu.Call(m)
	add := func(id int, text string, flags uintptr) {
		pAppendMenuW.Call(m, mfString|flags, uintptr(id), uintptr(unsafe.Pointer(u16(text))))
	}
	sep := func() { pAppendMenuW.Call(m, mfSeparator, 0, 0) }
	check := func(on bool) uintptr {
		if on {
			return mfChecked
		}
		return 0
	}

	add(cmdOpen, "Open "+appName, 0)
	sep()
	if a.update != nil {
		add(cmdInstallUpdate, fmt.Sprintf("Install update %s", a.update.Version), 0)
	}
	add(cmdCheckUpdate, "Check for updates", 0)
	add(cmdCaptureKeys, "Send Windows shortcuts to remote computers", check(a.cfg.captureKeys()))
	add(cmdAutostart, "Start with Windows", check(autostartEnabled()))
	add(cmdChangeServer, "Change server…", 0)
	sep()
	add(cmdQuit, "Quit", 0)

	var p point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	pSetForegroundWindow.Call(t.hwnd) // so the menu closes when you click elsewhere
	cmd, _, _ := pTrackPopupMenu.Call(m, tpmReturnCmd|tpmRightButton|tpmBottomAlign, uintptr(p.X), uintptr(p.Y), 0, t.hwnd, 0)
	pPostMessageW.Call(t.hwnd, wmNull, 0, 0)

	switch int(cmd) {
	case cmdOpen:
		a.showMain()
	case cmdChangeServer:
		a.changeServer()
	case cmdCaptureKeys:
		on := !a.cfg.captureKeys()
		a.cfg.CaptureKeys = &on
		_ = a.cfg.save()
	case cmdAutostart:
		if err := setAutostart(!autostartEnabled()); err != nil {
			messageBox(0, "Couldn't change the startup setting: "+err.Error(), appName, mbOK|mbIconError)
		}
	case cmdCheckUpdate:
		go a.checkUpdate(true)
	case cmdInstallUpdate:
		a.installUpdate()
	case cmdQuit:
		a.quit()
	}
}
