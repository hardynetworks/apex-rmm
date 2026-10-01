//go:build windows

// Apex RMM Desktop: a native Windows shell around the Apex RMM web dashboard.
//
// The dashboard itself is loaded from your Apex RMM server in WebView2 (the Edge engine built
// into Windows), so the app always matches the web version. On top of that it adds:
//   - remote-control sessions in their own windows, with keyboard capture (Win key, Alt+Tab,
//     Alt+Esc, Ctrl+Esc, Alt+F4 go to the remote computer instead of this one)
//   - a tray icon, Windows notifications for new alerts, start with Windows
//   - update checks against GitHub releases
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// version is set at build time with -ldflags "-X main.version=1.2.3".
var version = "dev"

const (
	hostClass   = "ApexRMMDesktopHost"
	windowClass = "ApexRMMDesktopWindow"
)

func init() { runtime.LockOSThread() } // all Win32 + WebView2 work happens on the main thread

type app struct {
	cfg        *config
	host       uintptr // hidden window: dispatch queue, tray callbacks, single-instance signal
	main       *appWindow
	windows    map[uintptr]*appWindow
	tray       *trayIcon
	background bool

	qmu       sync.Mutex
	queue     []func()
	embedding bool // inside WebView2 creation (which runs a nested message loop)

	quitting  bool
	notifyURL string // where a click on the last notification goes
	update    *release
}

var a *app

func main() {
	background := flag.Bool("background", false, "start minimized to the tray")
	flag.Parse()

	if f, err := os.OpenFile(filepath.Join(dataDir(), "desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if st, _ := f.Stat(); st != nil && st.Size() > 2<<20 {
			_ = f.Truncate(0)
		}
		log.SetOutput(io.MultiWriter(f, os.Stderr))
	}
	log.Printf("Apex RMM Desktop %s starting", version)

	// Only one instance: a second launch just brings the running one to the front.
	if _, err := windows.CreateMutex(nil, false, u16(`Local\ApexRMMDesktop`)); err == windows.ERROR_ALREADY_EXISTS {
		if h, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(u16(hostClass))), 0); h != 0 {
			pPostMessageW.Call(h, wmAppShow, 0, 0)
		}
		return
	}

	pSetProcessDpiAwareCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	pSetCurrentProcessExplicitAppUserModelID.Call(uintptr(unsafe.Pointer(u16("HardyNetworks.ApexRMM.Desktop"))))

	a = &app{cfg: loadConfig(), windows: map[uintptr]*appWindow{}, background: *background}

	icon := appIcon(true)
	registerClass(hostClass, hostProc, icon)
	registerClass(windowClass, windowProc, icon)
	a.host, _, _ = pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(u16(hostClass))), uintptr(unsafe.Pointer(u16(appName))),
		wsPopup, 0, 0, 0, 0, 0, 0, moduleHandle(), 0)

	a.tray = newTray(a.host)
	installKeyboardHook()
	defer uninstallKeyboardHook()

	a.main = a.openMain(!a.background)
	if a.main == nil {
		messageBox(0, "Apex RMM needs the Microsoft Edge WebView2 Runtime, which is missing or broken on this PC.\n\n"+
			"Install it from https://go.microsoft.com/fwlink/p/?LinkId=2124703 and start Apex RMM again.", appName, mbOK|mbIconError)
		a.tray.remove()
		return
	}
	go a.updateLoop()

	var m msgT
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	a.tray.remove()
	log.Printf("exiting")
}

// dispatch runs f on the UI thread.
func (a *app) dispatch(f func()) {
	a.qmu.Lock()
	a.queue = append(a.queue, f)
	a.qmu.Unlock()
	pPostMessageW.Call(a.host, wmAppDispatch, 0, 0)
}

func (a *app) drain() {
	if a.embedding { // try again once WebView2 creation has finished
		return
	}
	a.qmu.Lock()
	q := a.queue
	a.queue = nil
	a.qmu.Unlock()
	for _, f := range q {
		f()
	}
}

func (a *app) quit() {
	a.quitting = true
	for h := range a.windows {
		pDestroyWindow.Call(h)
	}
	pPostQuitMessage.Call(0)
}

func hostProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	switch msg {
	case wmAppDispatch:
		a.drain()
		return 0
	case wmAppShow:
		a.showMain()
		return 0
	case wmAppTray:
		a.tray.onEvent(uint32(lp & 0xFFFF))
		return 0
	}
	if a != nil && a.tray != nil && msg == a.tray.taskbarCreated && msg != 0 {
		a.tray.add() // Explorer restarted
		return 0
	}
	return defWindowProc(hwnd, msg, wp, lp)
}

func (a *app) showMain() {
	if a.main == nil {
		a.main = a.openMain(true)
		return
	}
	a.main.show()
}

func (a *app) openMain(visible bool) *appWindow {
	w := newWindow(kindMain, appName, 1400, 900, visible)
	if w == nil {
		return nil
	}
	if a.cfg.Server == "" {
		w.showSetup("")
	} else {
		w.navigate(a.cfg.Server + "/")
	}
	return w
}

// openSession opens a remote-control session in its own window (or focuses it if already open).
func (a *app) openSession(target string) {
	for _, w := range a.windows {
		if w.kind == kindSession && w.url == target {
			w.show()
			return
		}
	}
	w := newWindow(kindSession, "Remote control — "+appName, 1400, 900, true)
	if w == nil {
		return
	}
	w.navigate(target)
}

func (a *app) changeServer() {
	a.showMain()
	a.main.showSetup(a.cfg.Server)
}

func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }
