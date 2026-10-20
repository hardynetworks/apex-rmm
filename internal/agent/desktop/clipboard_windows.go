//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows clipboard belongs to the window station, so the SYSTEM helper shares it with the
// signed-in user. Setting it needs an owner window, and the owner's thread has to pump messages
// (other programs SendMessage it WM_DESTROYCLIPBOARD), so a dedicated locked thread owns a
// message-only window and runs a message loop; requests reach it on every WM_TIMER tick.
var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pOpenClipboard          = user32.NewProc("OpenClipboard")
	pCloseClipboard         = user32.NewProc("CloseClipboard")
	pEmptyClipboard         = user32.NewProc("EmptyClipboard")
	pGetClipboardData       = user32.NewProc("GetClipboardData")
	pSetClipboardData       = user32.NewProc("SetClipboardData")
	pGetClipboardSeq        = user32.NewProc("GetClipboardSequenceNumber")
	pRegisterClipboardFmt   = user32.NewProc("RegisterClipboardFormatW")
	pIsClipboardFmtAvail    = user32.NewProc("IsClipboardFormatAvailable")
	pCreateWindowEx         = user32.NewProc("CreateWindowExW")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pGetMessage             = user32.NewProc("GetMessageW")
	pDispatchMessage        = user32.NewProc("DispatchMessageW")
	pSetTimer               = user32.NewProc("SetTimer")
	pGlobalAlloc            = kernel32.NewProc("GlobalAlloc")
	pGlobalFree             = kernel32.NewProc("GlobalFree")
	pGlobalLock             = kernel32.NewProc("GlobalLock")
	pGlobalUnlock           = kernel32.NewProc("GlobalUnlock")
	pGlobalSize             = kernel32.NewProc("GlobalSize")
	preferredDropEffectName = windows.StringToUTF16Ptr("Preferred DropEffect")
)

const (
	cfUnicodeText = 13
	cfHDrop       = 15
	gmemMoveable  = 0x0002
	hwndMessage   = ^uintptr(2) // (HWND)-3
	wmTimer       = 0x0113
	maxClipText   = 1 << 20
)

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
	_       uint32
}

type winClipboard struct {
	reqs chan func(hwnd uintptr)
	quit chan struct{}
}

func newClipboard() (clipboard, error) {
	c := &winClipboard{reqs: make(chan func(uintptr), 16), quit: make(chan struct{})}
	ready := make(chan error, 1)
	go c.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return c, nil
}

func (c *winClipboard) loop(ready chan<- error) {
	runtime.LockOSThread() // the window and the message loop live on this thread for good
	cls := windows.StringToUTF16Ptr("STATIC")
	hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if hwnd == 0 {
		ready <- err
		return
	}
	pSetTimer.Call(hwnd, 1, 40, 0)
	ready <- nil
	var m winMsg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		if m.message == wmTimer && m.hwnd == hwnd {
		drain:
			for {
				select {
				case fn := <-c.reqs:
					fn(hwnd)
				case <-c.quit:
					pDestroyWindow.Call(hwnd)
					return
				default:
					break drain
				}
			}
			continue
		}
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// do runs fn on the clipboard thread and waits for it.
func (c *winClipboard) do(fn func(hwnd uintptr) error) error {
	done := make(chan error, 1)
	select {
	case c.reqs <- func(h uintptr) { done <- fn(h) }:
	case <-c.quit:
		return errors.New("clipboard closed")
	}
	return <-done
}

func openClipboard(hwnd uintptr) error {
	for i := 0; i < 10; i++ { // another program may hold it for a moment
		if r, _, _ := pOpenClipboard.Call(hwnd); r != 0 {
			return nil
		}
		windows.SleepEx(10, false)
	}
	return errors.New("the clipboard is busy")
}

func (c *winClipboard) Text() (string, error) {
	var out string
	err := c.do(func(hwnd uintptr) error {
		if r, _, _ := pIsClipboardFmtAvail.Call(cfUnicodeText); r == 0 {
			return errNoText
		}
		if err := openClipboard(hwnd); err != nil {
			return err
		}
		defer pCloseClipboard.Call()
		h, _, _ := pGetClipboardData.Call(cfUnicodeText)
		if h == 0 {
			return errNoText
		}
		p, _, _ := pGlobalLock.Call(h)
		if p == 0 {
			return errNoText
		}
		defer pGlobalUnlock.Call(h)
		size, _, _ := pGlobalSize.Call(h)
		n := int(size / 2)
		if n > maxClipText {
			n = maxClipText
		}
		u := unsafe.Slice((*uint16)(ptrFrom(p)), n)
		for i, ch := range u {
			if ch == 0 {
				u = u[:i]
				break
			}
		}
		out = string(utf16.Decode(u))
		return nil
	})
	return out, err
}

func globalBytes(b []byte) (uintptr, error) {
	h, _, err := pGlobalAlloc.Call(gmemMoveable, uintptr(len(b)))
	if h == 0 {
		return 0, err
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return 0, errors.New("GlobalLock failed")
	}
	copy(unsafe.Slice((*byte)(ptrFrom(p)), len(b)), b)
	pGlobalUnlock.Call(h)
	return h, nil
}

func utf16Bytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[2*i], b[2*i+1] = byte(v), byte(v>>8)
	}
	return b
}

func (c *winClipboard) SetText(s string) error {
	return c.do(func(hwnd uintptr) error {
		h, err := globalBytes(append(utf16Bytes(s), 0, 0))
		if err != nil {
			return err
		}
		if err := openClipboard(hwnd); err != nil {
			pGlobalFree.Call(h)
			return err
		}
		defer pCloseClipboard.Call()
		pEmptyClipboard.Call()
		if r, _, err := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
			pGlobalFree.Call(h)
			return err
		}
		return nil
	})
}

// SetFiles puts a file list on the clipboard (CF_HDROP, "copy"), so Ctrl+V pastes the files
// into Explorer, Outlook, Teams and most other programs.
func (c *winClipboard) SetFiles(paths []string) error {
	if len(paths) == 0 {
		return errors.New("no files")
	}
	// DROPFILES { DWORD pFiles; POINT pt; BOOL fNC; BOOL fWide; } then a double-NUL-terminated list.
	buf := make([]byte, 20)
	buf[0] = 20
	buf[16] = 1 // fWide
	for _, p := range paths {
		buf = append(buf, utf16Bytes(p)...)
		buf = append(buf, 0, 0)
	}
	buf = append(buf, 0, 0)
	return c.do(func(hwnd uintptr) error {
		h, err := globalBytes(buf)
		if err != nil {
			return err
		}
		effect, err := globalBytes([]byte{1, 0, 0, 0}) // DROPEFFECT_COPY
		if err != nil {
			pGlobalFree.Call(h)
			return err
		}
		if err := openClipboard(hwnd); err != nil {
			pGlobalFree.Call(h)
			pGlobalFree.Call(effect)
			return err
		}
		defer pCloseClipboard.Call()
		pEmptyClipboard.Call()
		if r, _, err := pSetClipboardData.Call(cfHDrop, h); r == 0 {
			pGlobalFree.Call(h)
			pGlobalFree.Call(effect)
			return err
		}
		if fmt, _, _ := pRegisterClipboardFmt.Call(uintptr(unsafe.Pointer(preferredDropEffectName))); fmt != 0 {
			if r, _, _ := pSetClipboardData.Call(fmt, effect); r == 0 {
				pGlobalFree.Call(effect)
			}
		} else {
			pGlobalFree.Call(effect)
		}
		return nil
	})
}

func (c *winClipboard) Seq() uint64 {
	r, _, _ := pGetClipboardSeq.Call()
	return uint64(r)
}

func (c *winClipboard) Close() {
	select {
	case <-c.quit:
	default:
		close(c.quit)
	}
}
