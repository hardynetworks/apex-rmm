//go:build darwin

package desktop

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/ebitengine/purego"
)

type cgPoint struct{ X, Y float64 }
type cgSize struct{ W, H float64 }
type cgRect struct {
	Origin cgPoint
	Size   cgSize
}

var (
	cgMainDisplayID              func() uint32
	cgGetActiveDisplayList       func(max uint32, displays *uint32, count *uint32) int32
	cgDisplayBounds              func(id uint32) cgRect
	cgDisplayCreateImage         func(id uint32) uintptr
	cgImageGetWidth              func(img uintptr) uintptr
	cgImageGetHeight             func(img uintptr) uintptr
	cgImageGetBytesPerRow        func(img uintptr) uintptr
	cgImageGetBitsPerPixel       func(img uintptr) uintptr
	cgImageGetDataProvider       func(img uintptr) uintptr
	cgDataProviderCopyData       func(p uintptr) uintptr
	cgImageRelease               func(img uintptr)
	cfDataGetBytePtr             func(d uintptr) uintptr
	cfDataGetLength              func(d uintptr) int
	cfRelease                    func(p uintptr)
	cgEventCreateMouseEvent      func(src uintptr, typ uint32, pt cgPoint, button uint32) uintptr
	cgEventCreateKeyboardEvent   func(src uintptr, key uint16, down bool) uintptr
	cgEventCreateScrollWheel2    func(src uintptr, units uint32, count uint32, w1, w2, w3 int32) uintptr
	cgEventSetFlags              func(ev uintptr, flags uint64)
	cgEventSetIntegerValueField  func(ev uintptr, field uint32, v int64)
	cgEventKeyboardSetUnicodeStr func(ev uintptr, n uint64, s *uint16)
	cgEventPost                  func(tap uint32, ev uintptr)
	cgPreflightCapture           func() bool
	cgRequestCapture             func() bool
	libErr                       error
)

func loadLibs() error {
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	reg := func(fptr any, lib uintptr, name string) {
		if _, err := purego.Dlsym(lib, name); err != nil {
			return // leave nil; callers check
		}
		purego.RegisterLibFunc(fptr, lib, name)
	}
	reg(&cgMainDisplayID, cg, "CGMainDisplayID")
	reg(&cgGetActiveDisplayList, cg, "CGGetActiveDisplayList")
	reg(&cgDisplayBounds, cg, "CGDisplayBounds")
	reg(&cgDisplayCreateImage, cg, "CGDisplayCreateImage")
	reg(&cgImageGetWidth, cg, "CGImageGetWidth")
	reg(&cgImageGetHeight, cg, "CGImageGetHeight")
	reg(&cgImageGetBytesPerRow, cg, "CGImageGetBytesPerRow")
	reg(&cgImageGetBitsPerPixel, cg, "CGImageGetBitsPerPixel")
	reg(&cgImageGetDataProvider, cg, "CGImageGetDataProvider")
	reg(&cgDataProviderCopyData, cg, "CGDataProviderCopyData")
	reg(&cgImageRelease, cg, "CGImageRelease")
	reg(&cfDataGetBytePtr, cf, "CFDataGetBytePtr")
	reg(&cfDataGetLength, cf, "CFDataGetLength")
	reg(&cfRelease, cf, "CFRelease")
	reg(&cgEventCreateMouseEvent, cg, "CGEventCreateMouseEvent")
	reg(&cgEventCreateKeyboardEvent, cg, "CGEventCreateKeyboardEvent")
	reg(&cgEventCreateScrollWheel2, cg, "CGEventCreateScrollWheelEvent2")
	reg(&cgEventSetFlags, cg, "CGEventSetFlags")
	reg(&cgEventSetIntegerValueField, cg, "CGEventSetIntegerValueField")
	reg(&cgEventKeyboardSetUnicodeStr, cg, "CGEventKeyboardSetUnicodeString")
	reg(&cgEventPost, cg, "CGEventPost")
	reg(&cgPreflightCapture, cg, "CGPreflightScreenCaptureAccess")
	reg(&cgRequestCapture, cg, "CGRequestScreenCaptureAccess")
	if cgMainDisplayID == nil || cgEventPost == nil || cgEventCreateMouseEvent == nil {
		return errors.New("CoreGraphics symbols missing")
	}
	return nil
}

const (
	evLeftDown     = 1
	evLeftUp       = 2
	evRightDown    = 3
	evRightUp      = 4
	evMoved        = 5
	evLeftDragged  = 6
	evRightDragged = 7
	evOtherDown    = 25
	evOtherUp      = 26
	evOtherDragged = 27

	flagShift   = 0x00020000
	flagControl = 0x00040000
	flagAlt     = 0x00080000
	flagCommand = 0x00100000
)

type macScreen struct {
	ids       []uint32
	displays  []Display
	cur       int
	frame     Frame
	scale     float64 // pixels per point
	lastPt    cgPoint
	buttons   [3]bool
	flags     uint64
	lastClick time.Time
	clickPt   cgPoint
	clicks    int64
	warning   string
	useCLI    bool
}

func openScreen() (Screen, error) {
	if libErr = loadLibs(); libErr != nil {
		return nil, libErr
	}
	s := &macScreen{scale: 1}
	if cgPreflightCapture != nil && !cgPreflightCapture() {
		if cgRequestCapture != nil {
			cgRequestCapture()
		}
		s.warning = "Screen Recording permission is not granted to the Apex agent. Approve it in System Settings > Privacy & Security > Screen Recording (and Accessibility for control)."
	}
	s.loadDisplays()
	if len(s.displays) == 0 {
		return nil, errors.New("no displays")
	}
	return s, nil
}

func (s *macScreen) loadDisplays() {
	ids := make([]uint32, 16)
	var n uint32
	if cgGetActiveDisplayList != nil && cgGetActiveDisplayList(16, &ids[0], &n) == 0 && n > 0 {
		ids = ids[:n]
	} else {
		ids = []uint32{cgMainDisplayID()}
	}
	main := cgMainDisplayID()
	s.ids = ids
	s.displays = nil
	for i, id := range ids {
		d := Display{Name: fmt.Sprintf("Display %d", i+1), Primary: id == main}
		if cgDisplayBounds != nil {
			b := cgDisplayBounds(id)
			d.X, d.Y, d.W, d.H = int(b.Origin.X), int(b.Origin.Y), int(b.Size.W), int(b.Size.H)
		}
		s.displays = append(s.displays, d)
		if d.Primary {
			s.cur = i
		}
	}
}

func (s *macScreen) Displays() []Display { return s.displays }
func (s *macScreen) Current() int        { return s.cur }
func (s *macScreen) Warning() string     { return s.warning }

func (s *macScreen) Select(i int) error {
	if i < 0 || i >= len(s.displays) {
		return errors.New("no such display")
	}
	s.cur = i
	return nil
}

func (s *macScreen) Capture() (*Frame, error) {
	if s.useCLI || cgDisplayCreateImage == nil {
		return s.captureCLI()
	}
	img := cgDisplayCreateImage(s.ids[s.cur])
	if img == 0 {
		s.useCLI = true
		return s.captureCLI()
	}
	defer cgImageRelease(img)
	w, h := int(cgImageGetWidth(img)), int(cgImageGetHeight(img))
	bpr := int(cgImageGetBytesPerRow(img))
	if cgImageGetBitsPerPixel(img) != 32 {
		return nil, errors.New("unsupported pixel format")
	}
	data := cgDataProviderCopyData(cgImageGetDataProvider(img))
	if data == 0 {
		return nil, errors.New("no image data")
	}
	defer cfRelease(data)
	n := cfDataGetLength(data)
	src := unsafe.Slice((*byte)(unsafe.Pointer(cfDataGetBytePtr(data))), n)
	if s.frame.W != w || s.frame.H != h {
		s.frame = Frame{W: w, H: h, Stride: w * 4, Pix: make([]byte, w*h*4)}
	}
	for y := 0; y < h && (y+1)*bpr <= n; y++ {
		copy(s.frame.Pix[y*w*4:(y+1)*w*4], src[y*bpr:y*bpr+w*4])
	}
	if d := s.displays[s.cur]; d.W > 0 {
		s.scale = float64(w) / float64(d.W)
	}
	return &s.frame, nil
}

// captureCLI is a slow fallback using /usr/sbin/screencapture.
func (s *macScreen) captureCLI() (*Frame, error) {
	tmp := filepath.Join(os.TempDir(), "apex-cap.png")
	defer os.Remove(tmp)
	if err := exec.Command("/usr/sbin/screencapture", "-x", "-C", "-D", fmt.Sprint(s.cur+1), "-t", "png", tmp).Run(); err != nil {
		return nil, err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	im, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := im.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Rect, im, b.Min, draw.Src)
	s.frame = Frame{W: b.Dx(), H: b.Dy(), Stride: b.Dx() * 4, Pix: rgba.Pix}
	for i := 0; i < len(s.frame.Pix); i += 4 { // RGBA -> BGRA
		s.frame.Pix[i], s.frame.Pix[i+2] = s.frame.Pix[i+2], s.frame.Pix[i]
	}
	if d := s.displays[s.cur]; d.W > 0 {
		s.scale = float64(b.Dx()) / float64(d.W)
	}
	return &s.frame, nil
}

func (s *macScreen) post(ev uintptr) {
	if ev == 0 {
		return
	}
	cgEventPost(0, ev)
	cfRelease(ev)
}

func (s *macScreen) Move(x, y int) {
	d := s.displays[s.cur]
	s.lastPt = cgPoint{X: float64(d.X) + float64(x)/s.scale, Y: float64(d.Y) + float64(y)/s.scale}
	typ, btn := uint32(evMoved), uint32(0)
	switch {
	case s.buttons[0]:
		typ = evLeftDragged
	case s.buttons[2]:
		typ, btn = evRightDragged, 1
	case s.buttons[1]:
		typ, btn = evOtherDragged, 2
	}
	ev := cgEventCreateMouseEvent(0, typ, s.lastPt, btn)
	if ev != 0 && cgEventSetFlags != nil && s.flags != 0 {
		cgEventSetFlags(ev, s.flags)
	}
	s.post(ev)
}

func (s *macScreen) Button(b int, down bool) {
	if b < 0 || b > 2 {
		return
	}
	s.buttons[b] = down
	var typ, btn uint32
	switch b {
	case 0:
		typ, btn = evLeftUp, 0
		if down {
			typ = evLeftDown
		}
	case 2:
		typ, btn = evRightUp, 1
		if down {
			typ = evRightDown
		}
	default:
		typ, btn = evOtherUp, 2
		if down {
			typ = evOtherDown
		}
	}
	if down {
		near := abs(s.lastPt.X-s.clickPt.X) < 4 && abs(s.lastPt.Y-s.clickPt.Y) < 4
		if time.Since(s.lastClick) < 400*time.Millisecond && near {
			s.clicks++
		} else {
			s.clicks = 1
		}
		s.lastClick, s.clickPt = time.Now(), s.lastPt
	}
	ev := cgEventCreateMouseEvent(0, typ, s.lastPt, btn)
	if ev != 0 {
		if cgEventSetIntegerValueField != nil {
			cgEventSetIntegerValueField(ev, 1 /*kCGMouseEventClickState*/, s.clicks)
		}
		if cgEventSetFlags != nil && s.flags != 0 {
			cgEventSetFlags(ev, s.flags)
		}
	}
	s.post(ev)
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func (s *macScreen) Wheel(dx, dy int) {
	if cgEventCreateScrollWheel2 == nil {
		return
	}
	s.post(cgEventCreateScrollWheel2(0, 1 /*line*/, 2, int32(-dy), int32(-dx), 0))
}

var modFlags = map[string]uint64{
	"ShiftLeft": flagShift, "ShiftRight": flagShift, "ControlLeft": flagControl, "ControlRight": flagControl,
	"AltLeft": flagAlt, "AltRight": flagAlt, "MetaLeft": flagCommand, "MetaRight": flagCommand, "OSLeft": flagCommand, "OSRight": flagCommand,
}

func (s *macScreen) Key(code string, down bool) {
	kc, ok := macKeys[code]
	if !ok {
		return
	}
	if f, isMod := modFlags[code]; isMod {
		if down {
			s.flags |= f
		} else {
			s.flags &^= f
		}
	}
	ev := cgEventCreateKeyboardEvent(0, kc, down)
	if ev != 0 && cgEventSetFlags != nil {
		cgEventSetFlags(ev, s.flags)
	}
	s.post(ev)
}

func (s *macScreen) Type(text string) {
	if cgEventKeyboardSetUnicodeStr == nil {
		return
	}
	for _, r := range text {
		u := utf16.Encode([]rune{r})
		for _, down := range []bool{true, false} {
			ev := cgEventCreateKeyboardEvent(0, 0, down)
			if ev == 0 {
				continue
			}
			cgEventKeyboardSetUnicodeStr(ev, uint64(len(u)), &u[0])
			s.post(ev)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *macScreen) CtrlAltDel() {}

func (s *macScreen) Close() {}
