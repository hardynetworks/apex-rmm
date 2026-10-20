package desktop

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type inMsg struct {
	T    string  `json:"t"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	B    int     `json:"b"`
	DX   float64 `json:"dx"`
	DY   float64 `json:"dy"`
	Code string  `json:"code"`
	Text string  `json:"text"`
	Q    int     `json:"q"`
	S    float64 `json:"s"`
	FPS  int     `json:"fps"`
	I    int     `json:"i"`
	V    int     `json:"v"` // viewer protocol version (2 = solid fills + adaptive quality)
}

// conn serialises writes to the websocket (frames, replies and file chunks come from several goroutines).
type conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *conn) write(mt int, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteMessage(mt, b)
}

func (c *conn) json(v any) error {
	b, _ := json.Marshal(v)
	return c.write(websocket.TextMessage, b)
}

// encodeJob hands a captured frame to the encoder goroutine together with the settings it
// should be encoded with (the encoder is owned by that goroutine).
type encodeJob struct {
	frame    *Frame
	scale    float64
	quality  int
	proto    int
	reset    bool
	adaptive bool
}

type encodeResult struct {
	sent bool
	err  error
}

const (
	maxInflight = 3                      // frames sent but not yet drawn by the viewer
	idleRate    = 125 * time.Millisecond // capture rate once nothing has happened for a while
	activeFor   = 2 * time.Second        // stay at full rate this long after input or a change
)

// Run is the entry point of the "desktop" helper process: it dials the server,
// streams the screen and injects input until the viewer disconnects.
//
// The main goroutine (locked to one OS thread, which Windows needs for desktop switching)
// captures the screen and injects input; scaling, encoding and sending happen on another
// goroutine so input is never stuck behind a slow encode, and file transfers run on their own.
func Run(server, session, token string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	u, err := url.Parse(server)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("session", session)
	q.Set("token", token)
	u.RawQuery = q.Encode()
	d := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 20 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, WriteBufferSize: 256 << 10}
	ws, _, err := d.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()
	c := &conn{ws: ws}

	scr, err := openScreen()
	if err != nil {
		_ = c.json(map[string]string{"t": "error", "error": "screen capture unavailable: " + err.Error()})
		return err
	}
	defer scr.Close()

	files := newFileManager(c)
	defer files.close()

	in := make(chan inMsg, 512)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.SetReadLimit(4 << 20)
		for {
			mt, b, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				files.binary(b)
				continue
			}
			var m inMsg
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			if isFileMsg(m.T) {
				files.control(b)
				continue
			}
			select {
			case in <- m:
			default: // drop input if we are hopelessly behind
			}
		}
	}()

	// Encoder goroutine.
	jobs := make(chan encodeJob, 2)
	results := make(chan encodeResult, 2)
	go func() {
		enc := &Encoder{}
		var scaled Frame
		lastW, lastH := 0, 0
		for j := range jobs {
			enc.Quality, enc.Proto, enc.Adaptive = j.quality, j.proto, j.adaptive
			if j.reset {
				enc.Reset()
			}
			f := Scale(j.frame, j.scale, &scaled)
			if f.W != lastW || f.H != lastH {
				lastW, lastH = f.W, f.H
				enc.Reset()
				if err := c.json(map[string]any{"t": "size", "w": f.W, "h": f.H, "scale": j.scale}); err != nil {
					results <- encodeResult{err: err}
					continue
				}
			}
			msg, n := enc.Encode(f)
			if n == 0 {
				results <- encodeResult{}
				continue
			}
			results <- encodeResult{sent: true, err: c.write(websocket.BinaryMessage, msg)}
		}
	}()
	defer close(jobs)

	var (
		bufs        [2]Frame // capture copies handed to the encoder (one encoding, one queued)
		nextBuf     int
		queued      int // jobs handed to the encoder and not finished yet
		inflight    int // frames sent and not yet acknowledged by the viewer
		scale       = 1.0
		quality     = 65
		fps         = 20
		proto       = 1
		reset       = true
		lastAck     = time.Now()
		lastCapture time.Time
		lastActive  = time.Now()
		waiting     bool // a capture was skipped because the viewer or encoder was behind
	)

	hello := func() error {
		return c.json(map[string]any{"t": "hello", "os": runtime.GOOS, "displays": scr.Displays(), "display": scr.Current(), "warning": scr.Warning(), "files": true})
	}
	if err := hello(); err != nil {
		return err
	}

	interval := func() time.Duration {
		iv := time.Second / time.Duration(fps)
		if time.Since(lastActive) > activeFor && iv < idleRate {
			iv = idleRate
		}
		return iv
	}
	// The first capture waits for the viewer's settings (they arrive right after hello), so the
	// first full frame is sent once, in the viewer's protocol and quality.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	nextAt := time.Now().Add(time.Second)
	schedule := func(at time.Time) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		nextAt = at
		timer.Reset(time.Until(at))
	}
	// After input, capture again soon (but no faster than the frame rate) so clicks and typing
	// show up without waiting for an idle-rate tick.
	expedite := func() {
		lastActive = time.Now()
		at := lastCapture.Add(time.Second / time.Duration(fps))
		if soon := time.Now().Add(8 * time.Millisecond); at.Before(soon) {
			at = soon
		}
		if at.Before(nextAt) {
			schedule(at)
		}
	}
	resume := func() {
		if waiting {
			waiting = false
			at := lastCapture.Add(time.Second / time.Duration(fps))
			if now := time.Now(); at.Before(now) {
				at = now
			}
			schedule(at)
		}
	}
	toScreen := func(v float64) int { return int(v / scale) }

	for {
		select {
		case <-done:
			return nil

		case r := <-results:
			queued--
			if r.err != nil {
				return r.err
			}
			if r.sent {
				inflight++
				lastActive = time.Now()
			}
			resume()

		case m := <-in:
			switch m.T {
			case "ack":
				if inflight > 0 {
					inflight--
				}
				lastAck = time.Now()
				resume()
				continue
			case "mm":
				scr.Move(toScreen(m.X), toScreen(m.Y))
			case "md", "mu":
				scr.Move(toScreen(m.X), toScreen(m.Y))
				scr.Button(m.B, m.T == "md")
			case "wh":
				scr.Wheel(int(m.DX), int(m.DY))
			case "kd", "ku":
				scr.Key(m.Code, m.T == "kd")
			case "type":
				scr.Type(m.Text)
			case "cad":
				scr.CtrlAltDel()
			case "opt":
				if m.Q >= 10 && m.Q <= 95 {
					quality = m.Q
				}
				if m.S >= 0.25 && m.S <= 1 {
					scale = m.S
				}
				if m.FPS >= 1 && m.FPS <= 60 {
					fps = m.FPS
				}
				if m.V >= 2 {
					proto = 2
				}
				reset = true
			case "display":
				if err := scr.Select(m.I); err == nil {
					reset = true
					_ = hello()
				}
			case "refresh":
				reset = true
				inflight = 0
				resume()
			}
			expedite()

		case <-timer.C:
			if queued >= len(bufs) || inflight+queued >= maxInflight {
				// The viewer (or the encoder) is behind: the next ack or finished encode resumes
				// capturing. If acks stopped entirely, recover after a while.
				if time.Since(lastAck) > 10*time.Second {
					inflight = 0
				}
				waiting = true
				schedule(time.Now().Add(time.Second))
				continue
			}
			lastCapture = time.Now()
			f, err := scr.Capture()
			schedule(lastCapture.Add(interval()))
			if err != nil {
				continue
			}
			b := &bufs[nextBuf]
			nextBuf = (nextBuf + 1) % len(bufs)
			if b.W != f.W || b.H != f.H || len(b.Pix) != len(f.Pix) {
				*b = Frame{W: f.W, H: f.H, Stride: f.Stride, Pix: make([]byte, len(f.Pix))}
			}
			b.Stride = f.Stride
			copy(b.Pix, f.Pix)
			jobs <- encodeJob{frame: b, scale: scale, quality: quality, proto: proto, reset: reset, adaptive: proto >= 2}
			reset = false
			queued++
		}
	}
}

func init() {
	log.SetFlags(log.LstdFlags)
}
