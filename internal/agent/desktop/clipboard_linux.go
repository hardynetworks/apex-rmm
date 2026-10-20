//go:build linux

package desktop

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// X11 CLIPBOARD selection on its own connection: we read it with ConvertSelection, own it to
// set it (answering other programs' SelectionRequests from our event loop), and count changes
// with XFixes selection-owner notifications.
type x11Clipboard struct {
	conn                                       *xgb.Conn
	win                                        xproto.Window
	aClip, aUTF8, aTargets, aProp, aText, aStr xproto.Atom
	aIncr                                      xproto.Atom

	mu     sync.Mutex
	owned  string
	isOurs bool
	seq    uint64
	notify chan xproto.SelectionNotifyEvent
}

func newClipboard() (clipboard, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	c := &x11Clipboard{conn: conn, notify: make(chan xproto.SelectionNotifyEvent, 4)}
	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)
	if c.win, err = xproto.NewWindowId(conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err := xproto.CreateWindowChecked(conn, 0, c.win, screen.Root, 0, 0, 1, 1, 0, xproto.WindowClassInputOnly, 0,
		xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		conn.Close()
		return nil, err
	}
	atom := func(name string) xproto.Atom {
		r, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
		if err != nil {
			return 0
		}
		return r.Atom
	}
	c.aClip, c.aUTF8, c.aTargets = atom("CLIPBOARD"), atom("UTF8_STRING"), atom("TARGETS")
	c.aProp, c.aText, c.aStr, c.aIncr = atom("APEX_CLIP"), atom("TEXT"), xproto.AtomString, atom("INCR")
	if xfixes.Init(conn) == nil {
		if _, err := xfixes.QueryVersion(conn, 5, 0).Reply(); err == nil {
			_ = xfixes.SelectSelectionInputChecked(conn, c.win, c.aClip, xfixes.SelectionEventMaskSetSelectionOwner).Check()
		}
	}
	go c.events()
	return c, nil
}

func (c *x11Clipboard) events() {
	for {
		ev, err := c.conn.WaitForEvent()
		if ev == nil && err == nil {
			return // connection closed
		}
		switch e := ev.(type) {
		case xproto.SelectionRequestEvent:
			c.serve(e)
		case xproto.SelectionNotifyEvent:
			select {
			case c.notify <- e:
			default:
			}
		case xproto.SelectionClearEvent:
			c.mu.Lock()
			c.isOurs = false
			c.mu.Unlock()
		case xfixes.SelectionNotifyEvent:
			if e.Owner != c.win {
				c.mu.Lock()
				c.seq++
				c.mu.Unlock()
			}
		}
	}
}

func (c *x11Clipboard) serve(e xproto.SelectionRequestEvent) {
	c.mu.Lock()
	text, ours := c.owned, c.isOurs
	c.mu.Unlock()
	prop := e.Property
	if prop == 0 {
		prop = e.Target
	}
	switch {
	case !ours:
		prop = 0
	case e.Target == c.aTargets:
		var b []byte
		for _, a := range []xproto.Atom{c.aTargets, c.aUTF8, c.aStr, c.aText} {
			b = append(b, byte(a), byte(a>>8), byte(a>>16), byte(a>>24))
		}
		xproto.ChangeProperty(c.conn, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, 4, b)
	case e.Target == c.aUTF8 || e.Target == c.aStr || e.Target == c.aText:
		xproto.ChangeProperty(c.conn, xproto.PropModeReplace, e.Requestor, prop, c.aUTF8, 8, uint32(len(text)), []byte(text))
	default:
		prop = 0
	}
	ev := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop}
	xproto.SendEvent(c.conn, false, e.Requestor, 0, string(ev.Bytes()))
}

func (c *x11Clipboard) Text() (string, error) {
	c.mu.Lock()
	if c.isOurs {
		t := c.owned
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	for len(c.notify) > 0 {
		<-c.notify
	}
	xproto.ConvertSelection(c.conn, c.win, c.aClip, c.aUTF8, c.aProp, xproto.TimeCurrentTime)
	select {
	case n := <-c.notify:
		if n.Property == 0 {
			return "", errNoText
		}
	case <-time.After(time.Second):
		return "", errors.New("the clipboard owner didn't answer")
	}
	r, err := xproto.GetProperty(c.conn, true, c.win, c.aProp, xproto.GetPropertyTypeAny, 0, (1<<20)/4).Reply()
	if err != nil {
		return "", err
	}
	if r.Type == c.aIncr {
		return "", errors.New("clipboard content too large")
	}
	return string(r.Value), nil
}

func (c *x11Clipboard) SetText(s string) error {
	c.mu.Lock()
	c.owned, c.isOurs = s, true
	c.mu.Unlock()
	xproto.SetSelectionOwner(c.conn, c.win, c.aClip, xproto.TimeCurrentTime)
	r, err := xproto.GetSelectionOwner(c.conn, c.aClip).Reply()
	if err != nil || r.Owner != c.win {
		return errors.New("couldn't take the clipboard")
	}
	return nil
}

func (c *x11Clipboard) SetFiles([]string) error { return errUnsupported }

func (c *x11Clipboard) Seq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

func (c *x11Clipboard) Close() { c.conn.Close() }

// resolveDrop on Linux: file managers don't expose the folder under the pointer, so drops go to
// the user's Desktop folder (or home).
func resolveDrop(gx, gy float64, foreground bool) (dropTarget, error) {
	home := userHome()
	if d := xdgDir(home, "XDG_DESKTOP_DIR"); d != "" {
		return dropTarget{Path: d, Label: filepath.Base(d), Kind: "desktop"}, nil
	}
	return dropTarget{Path: home, Label: "home folder", Kind: "desktop"}, nil
}

func xdgDir(home, key string) string {
	if f, err := os.Open(filepath.Join(home, ".config", "user-dirs.dirs")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(line, key+"="); ok {
				v = strings.ReplaceAll(strings.Trim(v, `"`), "$HOME", home)
				if st, err := os.Stat(v); err == nil && st.IsDir() {
					return v
				}
			}
		}
	}
	if d := filepath.Join(home, "Desktop"); dirExists(d) {
		return d
	}
	return ""
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
