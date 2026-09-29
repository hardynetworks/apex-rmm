package desktop

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime"
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
}

// Run is the entry point of the "desktop" helper process: it dials the server,
// streams the screen and injects input until the viewer disconnects.
func Run(server, session, token string) error {
	// Capture and input must stay on one OS thread (Windows desktop switching).
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

	scr, err := openScreen()
	if err != nil {
		msg, _ := json.Marshal(map[string]string{"t": "error", "error": "screen capture unavailable: " + err.Error()})
		_ = ws.WriteMessage(websocket.TextMessage, msg)
		return err
	}
	defer scr.Close()

	in := make(chan inMsg, 512)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.SetReadLimit(1 << 20)
		for {
			_, b, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m inMsg
			if json.Unmarshal(b, &m) == nil {
				select {
				case in <- m:
				default: // drop input if we are hopelessly behind
				}
			}
		}
	}()

	send := func(v any) error {
		b, _ := json.Marshal(v)
		_ = ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return ws.WriteMessage(websocket.TextMessage, b)
	}

	enc := &Encoder{Quality: 65}
	scale := 1.0
	fps := 15
	inflight := 0
	lastW, lastH := 0, 0
	var scaled Frame
	lastInput := time.Now()
	lastAck := time.Now()

	hello := func() error {
		return send(map[string]any{"t": "hello", "os": runtime.GOOS, "displays": scr.Displays(), "display": scr.Current(), "warning": scr.Warning()})
	}
	if err := hello(); err != nil {
		return err
	}

	tick := time.NewTicker(time.Second / time.Duration(fps))
	defer tick.Stop()
	toScreen := func(v float64) int { return int(v / scale) }

	for {
		select {
		case <-done:
			return nil
		case m := <-in:
			lastInput = time.Now()
			switch m.T {
			case "ack":
				if inflight > 0 {
					inflight--
				}
				lastAck = time.Now()
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
					enc.Quality = m.Q
				}
				if m.S >= 0.25 && m.S <= 1 {
					scale = m.S
				}
				if m.FPS >= 1 && m.FPS <= 30 && m.FPS != fps {
					fps = m.FPS
					tick.Reset(time.Second / time.Duration(fps))
				}
				enc.Reset()
			case "display":
				if err := scr.Select(m.I); err == nil {
					enc.Reset()
					_ = hello()
				}
			case "refresh":
				enc.Reset()
				inflight = 0
			}
		case <-tick.C:
			if inflight >= 2 {
				// viewer is behind; if acks stopped entirely, recover after a while
				if time.Since(lastAck) > 10*time.Second {
					inflight = 0
				}
				continue
			}
			f, err := scr.Capture()
			if err != nil {
				continue
			}
			f = Scale(f, scale, &scaled)
			if f.W != lastW || f.H != lastH {
				lastW, lastH = f.W, f.H
				enc.Reset()
				if err := send(map[string]any{"t": "size", "w": f.W, "h": f.H, "scale": scale}); err != nil {
					return err
				}
			}
			msg, n := enc.Encode(f)
			if n == 0 {
				continue
			}
			_ = ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return err
			}
			inflight++
			_ = lastInput
		}
	}
}

func init() {
	log.SetFlags(log.LstdFlags)
}
