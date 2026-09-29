//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	shcore = windows.NewLazySystemDLL("shcore.dll")
	sasdll = windows.NewLazySystemDLL("sas.dll")

	pGetDC                    = user32.NewProc("GetDC")
	pReleaseDC                = user32.NewProc("ReleaseDC")
	pGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	pSetCursorPos             = user32.NewProc("SetCursorPos")
	pSendInput                = user32.NewProc("SendInput")
	pOpenInputDesktop         = user32.NewProc("OpenInputDesktop")
	pSetThreadDesktop         = user32.NewProc("SetThreadDesktop")
	pCloseDesktop             = user32.NewProc("CloseDesktop")
	pGetUserObjectInformation = user32.NewProc("GetUserObjectInformationW")
	pEnumDisplayMonitors      = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfo           = user32.NewProc("GetMonitorInfoW")
	pSetDpiAwarenessContext   = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware       = user32.NewProc("SetProcessDPIAware")
	pSetProcessDpiAwareness   = shcore.NewProc("SetProcessDpiAwareness")

	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pBitBlt             = gdi32.NewProc("BitBlt")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pDeleteDC           = gdi32.NewProc("DeleteDC")

	pSendSAS = sasdll.NewProc("SendSAS")
)

const (
	smXVirtual  = 76
	smYVirtual  = 77
	smCXVirtual = 78
	smCYVirtual = 79
	srcCopy     = 0x00CC0020

	inputMouse    = 0
	inputKeyboard = 1

	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseXDown      = 0x0080
	mouseXUp        = 0x0100
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004
	keyScancode = 0x0008
)

type winInput struct {
	typ uint32
	_   uint32
	u   [4]uint64
}

type mouseInput struct {
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	_         uint32
	extra     uint64
}

type keybdInput struct {
	vk, scan uint16
	flags    uint32
	time     uint32
	_        uint32
	extra    uint64
}

type bitmapInfoHeader struct {
	size          uint32
	width, height int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xppm, yppm    int32
	clrUsed       uint32
	clrImportant  uint32
}

type monitorInfoEx struct {
	cbSize  uint32
	monitor windows.Rect
	work    windows.Rect
	flags   uint32
	device  [32]uint16
}

type winScreen struct {
	displays []Display
	cur      int
	desk     uintptr
	deskName string
	hdcScr   uintptr
	hdcMem   uintptr
	hbm      uintptr
	bits     uintptr
	bw, bh   int
	frame    Frame
}

func setDPIAware() {
	if pSetDpiAwarenessContext.Find() == nil {
		if r, _, _ := pSetDpiAwarenessContext.Call(^uintptr(3)); r != 0 { // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
			return
		}
	}
	if pSetProcessDpiAwareness.Find() == nil {
		pSetProcessDpiAwareness.Call(2)
		return
	}
	pSetProcessDPIAware.Call()
}

func openScreen() (Screen, error) {
	setDPIAware()
	s := &winScreen{}
	s.syncDesktop()
	s.loadDisplays()
	if len(s.displays) == 0 {
		return nil, errors.New("no displays found")
	}
	return s, nil
}

func (s *winScreen) loadDisplays() {
	var list []Display
	cb := syscall.NewCallback(func(hmon, hdc uintptr, r *windows.Rect, lp uintptr) uintptr {
		mi := monitorInfoEx{}
		mi.cbSize = uint32(unsafe.Sizeof(mi))
		if ret, _, _ := pGetMonitorInfo.Call(hmon, uintptr(unsafe.Pointer(&mi))); ret != 0 {
			list = append(list, Display{
				Name: fmt.Sprintf("Display %d (%s)", len(list)+1, windows.UTF16ToString(mi.device[:])),
				X:    int(mi.monitor.Left), Y: int(mi.monitor.Top),
				W: int(mi.monitor.Right - mi.monitor.Left), H: int(mi.monitor.Bottom - mi.monitor.Top),
				Primary: mi.flags&1 == 1,
			})
		}
		return 1
	})
	pEnumDisplayMonitors.Call(0, 0, cb, 0)
	if len(list) > 1 {
		x, _, _ := pGetSystemMetrics.Call(smXVirtual)
		y, _, _ := pGetSystemMetrics.Call(smYVirtual)
		w, _, _ := pGetSystemMetrics.Call(smCXVirtual)
		h, _, _ := pGetSystemMetrics.Call(smCYVirtual)
		list = append(list, Display{Name: "All displays", X: int(int32(x)), Y: int(int32(y)), W: int(int32(w)), H: int(int32(h))})
	}
	if len(list) == 0 {
		w, _, _ := pGetSystemMetrics.Call(0)
		h, _, _ := pGetSystemMetrics.Call(1)
		list = []Display{{Name: "Display 1", W: int(w), H: int(h), Primary: true}}
	}
	s.displays = list
	s.cur = 0
	for i, d := range list {
		if d.Primary {
			s.cur = i
		}
	}
}

func (s *winScreen) Displays() []Display { return s.displays }
func (s *winScreen) Current() int        { return s.cur }
func (s *winScreen) Warning() string     { return "" }

func (s *winScreen) Select(i int) error {
	if i < 0 || i >= len(s.displays) {
		return errors.New("no such display")
	}
	s.cur = i
	s.freeBitmap()
	return nil
}

func deskName(h uintptr) string {
	var buf [256]uint16
	var n uint32
	r, _, _ := pGetUserObjectInformation.Call(h, 2 /*UOI_NAME*/, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// syncDesktop follows the input desktop (Default, Winlogon for UAC/lock screen).
func (s *winScreen) syncDesktop() {
	h, _, _ := pOpenInputDesktop.Call(0, 0, 0x10000000 /*GENERIC_ALL*/)
	if h == 0 {
		return
	}
	name := deskName(h)
	if name == s.deskName && s.desk != 0 {
		pCloseDesktop.Call(h)
		return
	}
	if r, _, _ := pSetThreadDesktop.Call(h); r == 0 {
		pCloseDesktop.Call(h)
		return
	}
	if s.desk != 0 {
		pCloseDesktop.Call(s.desk)
	}
	s.desk, s.deskName = h, name
	s.freeDC()
}

func (s *winScreen) freeBitmap() {
	if s.hbm != 0 {
		pDeleteObject.Call(s.hbm)
		s.hbm, s.bits = 0, 0
	}
}

func (s *winScreen) freeDC() {
	s.freeBitmap()
	if s.hdcMem != 0 {
		pDeleteDC.Call(s.hdcMem)
		s.hdcMem = 0
	}
	if s.hdcScr != 0 {
		pReleaseDC.Call(0, s.hdcScr)
		s.hdcScr = 0
	}
}

func (s *winScreen) Capture() (*Frame, error) {
	s.syncDesktop()
	d := s.displays[s.cur]
	if s.hdcScr == 0 {
		s.hdcScr, _, _ = pGetDC.Call(0)
		s.hdcMem, _, _ = pCreateCompatibleDC.Call(s.hdcScr)
		if s.hdcScr == 0 || s.hdcMem == 0 {
			s.freeDC()
			return nil, errors.New("GetDC failed")
		}
	}
	if s.hbm == 0 || s.bw != d.W || s.bh != d.H {
		s.freeBitmap()
		bi := bitmapInfoHeader{width: int32(d.W), height: -int32(d.H), planes: 1, bitCount: 32}
		bi.size = uint32(unsafe.Sizeof(bi))
		s.hbm, _, _ = pCreateDIBSection.Call(s.hdcMem, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&s.bits)), 0, 0)
		if s.hbm == 0 || s.bits == 0 {
			return nil, errors.New("CreateDIBSection failed")
		}
		pSelectObject.Call(s.hdcMem, s.hbm)
		s.bw, s.bh = d.W, d.H
	}
	if r, _, _ := pBitBlt.Call(s.hdcMem, 0, 0, uintptr(d.W), uintptr(d.H), s.hdcScr, uintptr(int32(d.X)), uintptr(int32(d.Y)), srcCopy); r == 0 {
		s.freeDC() // desktop probably switched; rebuild next time
		return nil, errors.New("BitBlt failed")
	}
	n := d.W * d.H * 4
	if len(s.frame.Pix) != n {
		s.frame = Frame{W: d.W, H: d.H, Stride: d.W * 4, Pix: make([]byte, n)}
	}
	copy(s.frame.Pix, unsafe.Slice((*byte)(unsafe.Pointer(s.bits)), n))
	return &s.frame, nil
}

func sendInputs(in []winInput) {
	if len(in) == 0 {
		return
	}
	pSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
}

func mouse(flags uint32, data int32) winInput {
	in := winInput{typ: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&in.u[0])) = mouseInput{flags: flags, mouseData: uint32(data)}
	return in
}

func key(vk, scan uint16, flags uint32) winInput {
	in := winInput{typ: inputKeyboard}
	*(*keybdInput)(unsafe.Pointer(&in.u[0])) = keybdInput{vk: vk, scan: scan, flags: flags}
	return in
}

func (s *winScreen) Move(x, y int) {
	s.syncDesktop()
	d := s.displays[s.cur]
	pSetCursorPos.Call(uintptr(int32(d.X+x)), uintptr(int32(d.Y+y)))
}

func (s *winScreen) Button(b int, down bool) {
	s.syncDesktop()
	var f uint32
	var data int32
	switch b {
	case 0:
		f = mouseLeftUp
		if down {
			f = mouseLeftDown
		}
	case 1:
		f = mouseMiddleUp
		if down {
			f = mouseMiddleDown
		}
	case 2:
		f = mouseRightUp
		if down {
			f = mouseRightDown
		}
	case 3, 4:
		f = mouseXUp
		if down {
			f = mouseXDown
		}
		data = int32(b - 2) // XBUTTON1 = 1, XBUTTON2 = 2
	default:
		return
	}
	sendInputs([]winInput{mouse(f, data)})
}

func (s *winScreen) Wheel(dx, dy int) {
	s.syncDesktop()
	var in []winInput
	if dy != 0 {
		in = append(in, mouse(mouseWheel, int32(-dy*120)))
	}
	if dx != 0 {
		in = append(in, mouse(mouseHWheel, int32(dx*120)))
	}
	sendInputs(in)
}

func (s *winScreen) Key(code string, down bool) {
	s.syncDesktop()
	var fl uint32
	if !down {
		fl |= keyUp
	}
	switch code {
	case "Pause":
		sendInputs([]winInput{key(0x13, 0, fl)})
		return
	case "NumLock":
		sendInputs([]winInput{key(0x90, 0x45, fl|keyExtended)})
		return
	}
	ev, ok := evdev[code]
	if !ok {
		return
	}
	scan := uint16(ev)
	if ext, ok := winExtended[ev]; ok {
		scan = uint16(ext)
		fl |= keyExtended
	}
	sendInputs([]winInput{key(0, scan, fl|keyScancode)})
}

func (s *winScreen) Type(text string) {
	s.syncDesktop()
	var in []winInput
	for _, u := range utf16.Encode([]rune(text)) {
		if u == '\n' {
			in = append(in, key(0x0D, 0, 0), key(0x0D, 0, keyUp))
			continue
		}
		if u == '\r' {
			continue
		}
		in = append(in, key(0, u, keyUnicode), key(0, u, keyUnicode|keyUp))
	}
	sendInputs(in)
}

func (s *winScreen) CtrlAltDel() {
	if pSendSAS.Find() == nil {
		pSendSAS.Call(0) // FALSE: caller is a service
	}
}

func (s *winScreen) Close() {
	s.freeDC()
	if s.desk != 0 {
		pCloseDesktop.Call(s.desk)
	}
}
