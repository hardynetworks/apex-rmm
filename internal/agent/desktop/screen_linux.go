//go:build linux

package desktop

import (
	"errors"
	"fmt"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

type x11Screen struct {
	conn     *xgb.Conn
	root     xproto.Window
	rootW    int
	rootH    int
	displays []Display
	cur      int
	frame    Frame
	shiftKC  byte
}

func openScreen() (Screen, error) {
	conn, err := xgb.NewConn() // uses $DISPLAY and $XAUTHORITY
	if err != nil {
		return nil, fmt.Errorf("connect to X server: %w", err)
	}
	if err := xtest.Init(conn); err != nil {
		conn.Close()
		return nil, errors.New("X server lacks the XTEST extension (needed for input)")
	}
	setup := xproto.Setup(conn)
	sc := setup.DefaultScreen(conn)
	bpp := 0
	for _, f := range setup.PixmapFormats {
		if f.Depth == sc.RootDepth {
			bpp = int(f.BitsPerPixel)
		}
	}
	if bpp != 32 {
		conn.Close()
		return nil, fmt.Errorf("unsupported X pixel format (depth %d, %d bpp)", sc.RootDepth, bpp)
	}
	s := &x11Screen{conn: conn, root: sc.Root, rootW: int(sc.WidthInPixels), rootH: int(sc.HeightInPixels), shiftKC: byte(evdev["ShiftLeft"] + 8)}
	s.loadDisplays()
	return s, nil
}

func (s *x11Screen) loadDisplays() {
	s.displays = nil
	if randr.Init(s.conn) == nil {
		if rep, err := randr.GetMonitors(s.conn, s.root, true).Reply(); err == nil {
			for i, m := range rep.Monitors {
				name := fmt.Sprintf("Display %d", i+1)
				if an, err := xproto.GetAtomName(s.conn, m.Name).Reply(); err == nil {
					name = an.Name
				}
				s.displays = append(s.displays, Display{Name: name, X: int(m.X), Y: int(m.Y), W: int(m.Width), H: int(m.Height), Primary: m.Primary})
			}
		}
	}
	all := Display{Name: "All displays", W: s.rootW, H: s.rootH}
	if len(s.displays) <= 1 {
		s.displays = []Display{all}
		all.Primary = true
		s.displays[0].Primary = true
		s.cur = 0
		return
	}
	s.displays = append(s.displays, all)
	for i, d := range s.displays {
		if d.Primary {
			s.cur = i
			return
		}
	}
	s.cur = 0
}

func (s *x11Screen) Displays() []Display { return s.displays }
func (s *x11Screen) Current() int        { return s.cur }
func (s *x11Screen) Warning() string     { return "" }

func (s *x11Screen) Select(i int) error {
	if i < 0 || i >= len(s.displays) {
		return errors.New("no such display")
	}
	s.cur = i
	return nil
}

func (s *x11Screen) Capture() (*Frame, error) {
	d := s.displays[s.cur]
	if s.frame.W != d.W || s.frame.H != d.H {
		s.frame = Frame{W: d.W, H: d.H, Stride: d.W * 4, Pix: make([]byte, d.W*d.H*4)}
	}
	// Fetch in bands so each reply stays a reasonable size.
	band := max(1, (4<<20)/(d.W*4))
	for y := 0; y < d.H; y += band {
		h := min(band, d.H-y)
		rep, err := xproto.GetImage(s.conn, xproto.ImageFormatZPixmap, xproto.Drawable(s.root), int16(d.X), int16(d.Y+y), uint16(d.W), uint16(h), 0xffffffff).Reply()
		if err != nil {
			return nil, err
		}
		if len(rep.Data) < d.W*h*4 {
			return nil, errors.New("short image reply")
		}
		copy(s.frame.Pix[y*s.frame.Stride:], rep.Data[:d.W*h*4])
	}
	return &s.frame, nil
}

func (s *x11Screen) fake(typ byte, detail byte, x, y int16) {
	xtest.FakeInput(s.conn, typ, detail, 0, s.root, x, y, 0)
}

func (s *x11Screen) Move(x, y int) {
	d := s.displays[s.cur]
	s.fake(xproto.MotionNotify, 0, int16(d.X+clamp(x, 0, d.W-1)), int16(d.Y+clamp(y, 0, d.H-1)))
	s.conn.Sync()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (s *x11Screen) Button(b int, down bool) {
	btn := map[int]byte{0: 1, 1: 2, 2: 3, 3: 8, 4: 9}[b]
	if btn == 0 {
		return
	}
	t := byte(xproto.ButtonRelease)
	if down {
		t = xproto.ButtonPress
	}
	s.fake(t, btn, 0, 0)
	s.conn.Sync()
}

func (s *x11Screen) Wheel(dx, dy int) {
	click := func(b byte, n int) {
		for i := 0; i < n; i++ {
			s.fake(xproto.ButtonPress, b, 0, 0)
			s.fake(xproto.ButtonRelease, b, 0, 0)
		}
	}
	if dy > 0 {
		click(5, dy)
	} else if dy < 0 {
		click(4, -dy)
	}
	if dx > 0 {
		click(7, dx)
	} else if dx < 0 {
		click(6, -dx)
	}
	s.conn.Sync()
}

func (s *x11Screen) Key(code string, down bool) {
	c, ok := evdev[code]
	if !ok {
		return
	}
	t := byte(xproto.KeyRelease)
	if down {
		t = xproto.KeyPress
	}
	s.fake(t, byte(c+8), 0, 0)
	s.conn.Sync()
}

func (s *x11Screen) Type(text string) {
	for _, r := range text {
		k, ok := usASCII[r]
		if !ok {
			continue
		}
		kc := byte(k[0] + 8)
		if k[1] == 1 {
			s.fake(xproto.KeyPress, s.shiftKC, 0, 0)
		}
		s.fake(xproto.KeyPress, kc, 0, 0)
		s.fake(xproto.KeyRelease, kc, 0, 0)
		if k[1] == 1 {
			s.fake(xproto.KeyRelease, s.shiftKC, 0, 0)
		}
		s.conn.Sync()
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *x11Screen) CtrlAltDel() {
	for _, k := range []string{"ControlLeft", "AltLeft", "Delete"} {
		s.Key(k, true)
	}
	for _, k := range []string{"Delete", "AltLeft", "ControlLeft"} {
		s.Key(k, false)
	}
}

func (s *x11Screen) Close() { s.conn.Close() }
