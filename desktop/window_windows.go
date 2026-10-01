//go:build windows

package main

import (
	"log"
	"path/filepath"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
)

type windowKind int

const (
	kindMain    windowKind = iota // the dashboard; closing hides it to the tray
	kindSession                   // a remote-control session; closing really closes it
)

type appWindow struct {
	hwnd    uintptr
	kind    windowKind
	browser *edge.Chromium
	url     string
	capture bool // page asked for keyboard capture (remote session is live)
}

func newWindow(kind windowKind, title string, width, height int, visible bool) *appWindow {
	w, h := scale(width), scale(height)
	sw, _, _ := pGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := pGetSystemMetrics.Call(smCyScreen)
	if int(sw) > 0 && w > int(sw)-40 {
		w = int(sw) - 40
	}
	if int(sh) > 0 && h > int(sh)-80 {
		h = int(sh) - 80
	}
	x, y := uintptr(cwUseDefault), uintptr(cwUseDefault)
	if sw > 0 && sh > 0 {
		x, y = uintptr((int(sw)-w)/2), uintptr((int(sh)-h)/2)
		if kind == kindSession { // cascade a little so sessions don't stack exactly
			off := uintptr(scale(28 * (len(a.windows) % 6)))
			x, y = x+off, y+off
		}
	}
	hwnd, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(u16(windowClass))), uintptr(unsafe.Pointer(u16(title))),
		wsOverlappedWindow, x, y, uintptr(w), uintptr(h), 0, 0, moduleHandle(), 0)
	if hwnd == 0 {
		return nil
	}
	win := &appWindow{hwnd: hwnd, kind: kind}
	a.windows[hwnd] = win
	if big := appIcon(true); big != 0 {
		pSendMessageW.Call(hwnd, wmSetIcon, 1, big)
	}

	b := edge.NewChromium()
	b.DataPath = filepath.Join(dataDir(), "WebView2")
	b.MessageCallback = win.onMessage
	b.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateAllow)
	win.browser = b

	if visible {
		win.show()
	}
	a.embedding = true
	ok := b.Embed(hwnd)
	a.embedding = false
	pPostMessageW.Call(a.host, wmAppDispatch, 0, 0) // run anything queued while we were busy
	if !ok {
		log.Printf("WebView2 could not be created")
		delete(a.windows, hwnd)
		pDestroyWindow.Call(hwnd)
		return nil
	}
	if s, err := b.GetSettings(); err == nil {
		dev := version == "dev"
		_ = s.PutAreDevToolsEnabled(dev)
		_ = s.PutIsStatusBarEnabled(false)
		_ = s.PutIsZoomControlEnabled(kind == kindMain)
		if kind == kindSession {
			// F5, Ctrl+R, Ctrl+F, Ctrl+P … go to the remote computer instead of reloading/searching here.
			_ = s.PutAreBrowserAcceleratorKeysEnabled(dev)
		}
	}
	b.Init(bridgeScript(kind))
	b.Resize()
	return win
}

func (w *appWindow) navigate(url string) {
	w.url = url
	w.browser.Navigate(url)
}

func (w *appWindow) showSetup(current string) {
	w.url = ""
	w.browser.NavigateToString(setupPage(current))
}

func (w *appWindow) show() {
	if r, _, _ := pIsIconic.Call(w.hwnd); r != 0 {
		pShowWindow.Call(w.hwnd, swRestore)
	} else {
		pShowWindow.Call(w.hwnd, swShow)
	}
	pSetForegroundWindow.Call(w.hwnd)
	w.browser.Focus()
}

func (w *appWindow) setTitle(t string) { pSetWindowTextW.Call(w.hwnd, uintptr(unsafe.Pointer(u16(t)))) }

func (w *appWindow) eval(js string) {
	if w.browser != nil {
		w.browser.Eval(js)
	}
}

func windowProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	w := a.windows[hwnd]
	if w == nil {
		return defWindowProc(hwnd, msg, wp, lp)
	}
	switch msg {
	case wmSize:
		if w.browser != nil {
			w.browser.Resize()
		}
	case wmMove:
		if w.browser != nil {
			_ = w.browser.NotifyParentWindowPositionChanged()
		}
	case wmActivate:
		if wp&0xFFFF != waInactive && w.browser != nil {
			w.browser.Focus()
		}
	case wmGetMinMaxInfo:
		mmi := (*minMaxInfo)(ptr(lp))
		mmi.PtMinTrackSize = point{int32(scale(480)), int32(scale(360))}
		return 0
	case wmClose:
		if w.kind == kindMain && !a.quitting {
			pShowWindow.Call(hwnd, swHide) // keep running in the tray
			a.tray.hintOnce()
			return 0
		}
		if w.browser != nil {
			w.browser.Navigate("about:blank") // ends the remote session right away
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		delete(a.windows, hwnd)
		if w == a.main {
			a.main = nil
		}
		return 0
	}
	return defWindowProc(hwnd, msg, wp, lp)
}
