//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassExW                        = user32.NewProc("RegisterClassExW")
	pCreateWindowExW                         = user32.NewProc("CreateWindowExW")
	pDefWindowProcW                          = user32.NewProc("DefWindowProcW")
	pShowWindow                              = user32.NewProc("ShowWindow")
	pSetForegroundWindow                     = user32.NewProc("SetForegroundWindow")
	pDestroyWindow                           = user32.NewProc("DestroyWindow")
	pPostMessageW                            = user32.NewProc("PostMessageW")
	pGetMessageW                             = user32.NewProc("GetMessageW")
	pTranslateMessage                        = user32.NewProc("TranslateMessage")
	pDispatchMessageW                        = user32.NewProc("DispatchMessageW")
	pPostQuitMessage                         = user32.NewProc("PostQuitMessage")
	pSetWindowTextW                          = user32.NewProc("SetWindowTextW")
	pLoadCursorW                             = user32.NewProc("LoadCursorW")
	pSetWindowsHookExW                       = user32.NewProc("SetWindowsHookExW")
	pCallNextHookEx                          = user32.NewProc("CallNextHookEx")
	pUnhookWindowsHookEx                     = user32.NewProc("UnhookWindowsHookEx")
	pGetForegroundWindow                     = user32.NewProc("GetForegroundWindow")
	pGetAsyncKeyState                        = user32.NewProc("GetAsyncKeyState")
	pCreatePopupMenu                         = user32.NewProc("CreatePopupMenu")
	pAppendMenuW                             = user32.NewProc("AppendMenuW")
	pTrackPopupMenu                          = user32.NewProc("TrackPopupMenu")
	pDestroyMenu                             = user32.NewProc("DestroyMenu")
	pGetCursorPos                            = user32.NewProc("GetCursorPos")
	pMessageBoxW                             = user32.NewProc("MessageBoxW")
	pFindWindowW                             = user32.NewProc("FindWindowW")
	pIsWindowVisible                         = user32.NewProc("IsWindowVisible")
	pIsIconic                                = user32.NewProc("IsIconic")
	pSendMessageW                            = user32.NewProc("SendMessageW")
	pRegisterWindowMessageW                  = user32.NewProc("RegisterWindowMessageW")
	pGetDpiForSystem                         = user32.NewProc("GetDpiForSystem")
	pSetProcessDpiAwareCtx                   = user32.NewProc("SetProcessDpiAwarenessContext")
	pGetSystemMetrics                        = user32.NewProc("GetSystemMetrics")
	pShellNotifyIconW                        = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW                           = shell32.NewProc("ShellExecuteW")
	pExtractIconExW                          = shell32.NewProc("ExtractIconExW")
	pGetModuleHandleW                        = kernel32.NewProc("GetModuleHandleW")
	pSetCurrentProcessExplicitAppUserModelID = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
)

const (
	wmDestroy       = 0x0002
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmActivate      = 0x0006
	wmClose         = 0x0010
	wmQuit          = 0x0012
	wmGetMinMaxInfo = 0x0024
	wmSetIcon       = 0x0080
	wmNull          = 0x0000
	wmCommand       = 0x0111
	wmContextMenu   = 0x007B
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmUser          = 0x0400
	wmApp           = 0x8000

	wmAppDispatch = wmApp + 1 // run queued functions on the UI thread
	wmAppShow     = wmApp + 2 // a second instance asks us to show the main window
	wmAppTray     = wmApp + 3 // tray icon callback

	ninSelect           = wmUser + 0
	ninBalloonUserClick = wmUser + 5

	wsOverlappedWindow = 0x00CF0000
	wsPopup            = 0x80000000
	cwUseDefault       = 0x80000000

	swHide       = 0
	swShowNormal = 1
	swShow       = 5
	swRestore    = 9
	waInactive   = 0
	smCxScreen   = 0
	smCyScreen   = 1

	whKeyboardLL  = 13
	llkhfAltDown  = 0x20
	llkhfUp       = 0x80
	llkhfInjected = 0x10
	vkTab         = 0x09
	vkEscape      = 0x1B
	vkSpace       = 0x20
	vkLWin        = 0x5B
	vkRWin        = 0x5C
	vkF4          = 0x73
	vkControl     = 0x11

	mfString       = 0x0000
	mfSeparator    = 0x0800
	mfChecked      = 0x0008
	mfGrayed       = 0x0001
	tpmReturnCmd   = 0x0100
	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	mbOK           = 0x0
	mbIconError    = 0x10
	mbIconInfo     = 0x40
	mbYesNo        = 0x4
	mbIconQuestion = 0x20
	idYes          = 6

	nimAdd             = 0
	nimModify          = 1
	nimDelete          = 2
	nimSetVersion      = 4
	nifMessage         = 0x01
	nifIcon            = 0x02
	nifTip             = 0x04
	nifInfo            = 0x10
	nifShowTip         = 0x80
	niifUser           = 0x04
	niifLargeIcon      = 0x20
	notifyIconVersion4 = 4
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct{ X, Y int32 }

type msgT struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type minMaxInfo struct {
	PtReserved, PtMaxSize, PtMaxPosition, PtMinTrackSize, PtMaxTrackSize point
}

type kbdLLHook struct {
	VkCode    uint32
	ScanCode  uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

func u16(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }

func copyU16(dst []uint16, s string) {
	src, _ := windows.UTF16FromString(s)
	if len(src) > len(dst) {
		src = append(src[:len(dst)-1], 0)
	}
	copy(dst, src)
}

// ptr converts a pointer that Windows passed us as a uintptr (e.g. a callback's lParam).
func ptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

func moduleHandle() uintptr { h, _, _ := pGetModuleHandleW.Call(0); return h }

func defWindowProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wp, lp)
	return r
}

func registerClass(name string, proc func(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr, icon uintptr) {
	cursor, _, _ := pLoadCursorW.Call(0, 32512) // IDC_ARROW
	wc := wndClassExW{
		LpfnWndProc:   syscall.NewCallback(proc),
		HInstance:     moduleHandle(),
		HIcon:         icon,
		HIconSm:       icon,
		HCursor:       cursor,
		HbrBackground: 6, // COLOR_WINDOW+1
		LpszClassName: u16(name),
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
}

func messageBox(hwnd uintptr, text, title string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16(title))), flags)
	return int(r)
}

// shellOpen opens a URL or file with its default handler.
func shellOpen(target string, args string) {
	var a uintptr
	if args != "" {
		a = uintptr(unsafe.Pointer(u16(args)))
	}
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(target))), a, 0, swShowNormal)
}

func scale(v int) int {
	dpi, _, err := pGetDpiForSystem.Call()
	if err != nil && dpi == 0 {
		return v
	}
	if dpi == 0 {
		dpi = 96
	}
	return v * int(dpi) / 96
}

// appIcon returns the icon embedded in our own executable (or the default application icon).
func appIcon(large bool) uintptr {
	exe, _ := windows.UTF16PtrFromString(exePath())
	var big, small uintptr
	n, _, _ := pExtractIconExW.Call(uintptr(unsafe.Pointer(exe)), 0, uintptr(unsafe.Pointer(&big)), uintptr(unsafe.Pointer(&small)), 1)
	if n != 0 {
		if large && big != 0 {
			return big
		}
		if small != 0 {
			return small
		}
	}
	h, _, _ := user32.NewProc("LoadIconW").Call(0, 32512) // IDI_APPLICATION
	return h
}
