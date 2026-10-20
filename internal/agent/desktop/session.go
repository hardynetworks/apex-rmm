package desktop

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
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
	V    int     `json:"v"`    // viewer protocol version (2 = solid fills + adaptive quality)
	VC   bool    `json:"vc"`   // the viewer can decode H.264
	BW   int     `json:"bw"`   // bandwidth limit in kbit/s (0 = automatic, -1 = unchanged)
	Mode string  `json:"mode"` // "images" forces JPEG tiles
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
// should be encoded with (the encoders are owned by that goroutine).
type encodeJob struct {
	frame    *Frame
	scale    float64
	quality  int
	proto    int
	reset    bool
	adaptive bool
	video    bool // H.264 instead of tiles
	bitrate  int
	fps      int
	key      bool // force a keyframe
	at       time.Time
}

type encodeResult struct {
	sent     bool
	video    bool
	rtc      bool // went over WebRTC (no per-frame acks)
	seq      uint32
	bytes    int
	encode   time.Duration
	err      error // the connection failed
	videoErr error // the video encoder failed: fall back to tiles
}

const (
	maxInflight  = 3                      // tile updates sent but not yet drawn by the viewer
	idleRate     = 125 * time.Millisecond // capture rate once nothing has happened for a while
	activeFor    = 2 * time.Second        // stay at full rate this long after input or a change
	minBitrate   = 250_000
	autoMaxRate  = 30_000_000
	refineFrames = 6 // video frames still encoded after the screen stops changing, to sharpen it
)

// Video frames: 0x03 | u8 flags (1 = keyframe) | u32 seq | H.264 Annex B access unit.
const msgVideo = 3

// Run is the entry point of the "desktop" helper process: it dials the server,
// streams the screen and injects input until the viewer disconnects.
//
// The main goroutine (locked to one OS thread, which Windows needs for desktop switching)
// captures the screen and injects input; scaling, encoding and sending happen on another
// goroutine so input is never stuck behind a slow encode, and file transfers run on their own.
//
// Two encodings: H.264 video (OpenH264, when the viewer can decode it) with bandwidth
// estimation from per-frame acknowledgements, or dirty JPEG tiles as a fallback.
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
	rtcOffers := make(chan string, 2)
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
			if m.T == "rtc-stop" { // the viewer turned the direct path off
				select {
				case rtcOffers <- "":
				default:
				}
				continue
			}
			if m.T == "rtc-offer" {
				if sig, ok := parseRTC(b); ok {
					select {
					case rtcOffers <- sig.SDP:
					default:
					}
				}
				continue
			}
			select {
			case in <- m:
			default: // drop input if we are hopelessly behind
			}
		}
	}()

	// The video sink: the session websocket, or WebRTC while a direct path is up.
	var (
		rtcMu   sync.Mutex
		rtc     *rtcPeer // current peer connection (may still be connecting)
		rtcLive *rtcPeer // set while video goes over WebRTC
	)
	defer func() {
		rtcMu.Lock()
		if rtc != nil {
			rtc.Close()
		}
		rtcMu.Unlock()
	}()
	sink := func(b []byte, key bool) (bool, error) {
		rtcMu.Lock()
		p := rtcLive
		rtcMu.Unlock()
		if p != nil {
			if err := p.WriteFrame(b[6:]); err == nil {
				return true, nil
			}
		}
		return false, c.write(websocket.BinaryMessage, b)
	}
	push := func(m inMsg) {
		select {
		case in <- m:
		default:
		}
	}
	startRTC := func(offer string) {
		if offer == "" {
			rtcMu.Lock()
			old, wasLive := rtc, rtcLive != nil
			rtc, rtcLive = nil, nil
			rtcMu.Unlock()
			if old != nil {
				old.Close()
			}
			if wasLive {
				push(inMsg{T: "rtc-down"})
			}
			return
		}
		var peer *rtcPeer
		peer, answer, err := newRTCPeer(offer,
			func(b []byte) { // input over the data channel
				var m inMsg
				if json.Unmarshal(b, &m) == nil {
					push(m)
				}
			},
			func() { push(inMsg{T: "key"}) },
			func(up bool) {
				rtcMu.Lock()
				if rtc == peer {
					if up {
						rtcLive = peer
					} else {
						rtcLive = nil
					}
				}
				rtcMu.Unlock()
				if up {
					push(inMsg{T: "rtc-up"})
				} else {
					push(inMsg{T: "rtc-down"})
				}
			})
		if err != nil {
			log.Printf("webrtc: %v", err)
			_ = c.json(map[string]any{"t": "rtc-off", "error": err.Error()})
			return
		}
		rtcMu.Lock()
		old := rtc
		rtc, rtcLive = peer, nil
		rtcMu.Unlock()
		if old != nil {
			old.Close()
		}
		_ = c.json(map[string]any{"t": "rtc-answer", "sdp": answer})
	}
	go func() {
		for o := range rtcOffers {
			startRTC(o)
		}
	}()

	// Encoder goroutine.
	jobs := make(chan encodeJob, 2)
	results := make(chan encodeResult, 2)
	go func() {
		enc := &Encoder{}
		var venc *H264Encoder
		defer func() {
			if venc != nil {
				venc.Close()
			}
		}()
		var scaled Frame
		var prev []byte
		var seq uint32
		refine := 0
		lastW, lastH, lastVideo := 0, 0, false
		for j := range jobs {
			t0 := time.Now()
			f := Scale(j.frame, j.scale, &scaled)
			if j.video {
				w, h := f.W&^1, f.H&^1
				if venc == nil || venc.W != w || venc.H != h {
					if venc != nil {
						venc.Close()
						venc = nil
					}
					lib, _ := VideoCodec()
					if lib == nil {
						results <- encodeResult{videoErr: errors.New("encoder not available")}
						continue
					}
					var err error
					if venc, err = NewH264Encoder(lib, w, h, j.bitrate, float64(j.fps)); err != nil {
						results <- encodeResult{videoErr: err}
						continue
					}
					prev = prev[:0]
					refine = 0
				}
				if !lastVideo || lastW != w || lastH != h {
					lastW, lastH, lastVideo = w, h, true
					if err := c.json(map[string]any{"t": "size", "w": w, "h": h, "scale": j.scale, "video": true}); err != nil {
						results <- encodeResult{err: err}
						continue
					}
					j.key = true
				}
				venc.SetRate(j.bitrate, float64(j.fps))
				n := f.Stride * f.H
				if len(prev) != n || !bytes.Equal(prev, f.Pix[:n]) {
					prev = append(prev[:0], f.Pix[:n]...)
					refine = refineFrames
				} else if refine > 0 {
					refine--
				} else if !j.key {
					results <- encodeResult{}
					continue
				}
				if j.key {
					venc.ForceKeyframe()
				}
				data, key, err := venc.Encode(f, j.at.UnixMilli())
				if err != nil {
					venc.Close()
					venc = nil
					lastVideo = false
					results <- encodeResult{videoErr: err}
					continue
				}
				if data == nil { // rate control skipped this frame
					results <- encodeResult{encode: time.Since(t0)}
					continue
				}
				seq++
				msg := make([]byte, 6+len(data))
				msg[0] = msgVideo
				if key {
					msg[1] = 1
				}
				binary.BigEndian.PutUint32(msg[2:], seq)
				copy(msg[6:], data)
				encDur := time.Since(t0)
				viaRTC, err := sink(msg, key)
				results <- encodeResult{sent: true, video: true, rtc: viaRTC, seq: seq, bytes: len(msg), encode: encDur, err: err}
				continue
			}

			// Tiles.
			if venc != nil {
				venc.Close()
				venc = nil
			}
			enc.Quality, enc.Proto, enc.Adaptive = j.quality, j.proto, j.adaptive
			if j.reset {
				enc.Reset()
			}
			if lastVideo || f.W != lastW || f.H != lastH {
				lastW, lastH, lastVideo = f.W, f.H, false
				enc.Reset()
				if err := c.json(map[string]any{"t": "size", "w": f.W, "h": f.H, "scale": j.scale}); err != nil {
					results <- encodeResult{err: err}
					continue
				}
			}
			msg, n := enc.Encode(f)
			if n == 0 {
				results <- encodeResult{encode: time.Since(t0)}
				continue
			}
			encDur := time.Since(t0)
			results <- encodeResult{sent: true, bytes: len(msg), encode: encDur, err: c.write(websocket.BinaryMessage, msg)}
		}
	}()
	defer close(jobs)

	var (
		bufs        [2]Frame // capture copies handed to the encoder (one encoding, one queued)
		nextBuf     int
		queued      int // jobs handed to the encoder and not finished yet
		inflight    int // tile updates sent and not yet acknowledged by the viewer
		scale       = 1.0
		quality     = 65
		fps         = 20
		proto       = 1
		reset       = true
		lastAck     = time.Now()
		lastCapture time.Time
		lastActive  = time.Now()
		waiting     bool // a capture was skipped because the viewer or encoder was behind

		// video
		wantVideo     bool // the viewer can decode H.264 and didn't ask for images
		videoOff      string
		bwCap         int // bit/s chosen by the viewer, 0 = automatic
		bitrate       = 1_500_000
		needKey       bool
		sentAt        = map[uint32]sentFrame{}
		srtt          time.Duration
		baseRTT       rttWindow
		sentBytes     int // since the last rate check
		ackedBytes    int
		sendFps       float64 // frames actually sent per second (smoothed)
		tokens        float64 // pacing: bytes we may send now (video)
		avgFrame      float64 // smoothed video frame size in bytes
		tokensAt      = time.Now()
		lastRateCheck = time.Now()
		lastDecrease  time.Time
		congested     bool
		rtcOn         bool // video is going over WebRTC
		everCongested bool

		// stats
		stCap, stEnc time.Duration
		stFrames     int
		stBytes      int
		stCaptures   int
		lastStats    = time.Now()
	)
	maxRate := func() int {
		if bwCap > 0 {
			return bwCap
		}
		return autoMaxRate
	}
	videoOn := func() bool {
		if !wantVideo || videoOff != "" {
			return false
		}
		lib, _ := VideoCodec()
		return lib != nil
	}
	videoWindow := func() int { return max(4, fps/4) } // about 250 ms of frames in flight

	hello := func() error {
		_, codec := VideoCodec()
		return c.json(map[string]any{"t": "hello", "os": runtime.GOOS, "displays": scr.Displays(), "display": scr.Current(), "warning": scr.Warning(), "files": true, "video": codec})
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
	// first frame is sent once, in the viewer's format and quality.
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
		if soon := time.Now().Add(4 * time.Millisecond); at.Before(soon) {
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
	// Bandwidth estimation (video): back off when the round trip grows well beyond the
	// connection's base RTT (frames are queueing somewhere), probe upwards when it's quiet.
	//
	// Two signals: the round trip growing well past the connection's base RTT (frames are
	// queueing in some buffer), and frames going out faster than the viewer acknowledges them
	// (the link can't carry what we send). Either one drops the target to what is actually
	// getting through; a quiet connection lets it climb again.
	adjustRate := func() {
		now := time.Now()
		elapsed := now.Sub(lastRateCheck)
		if elapsed < 500*time.Millisecond {
			return
		}
		lastRateCheck = now
		sent := float64(sentBytes) * 8 / elapsed.Seconds()
		delivered := float64(ackedBytes) * 8 / elapsed.Seconds()
		sentBytes, ackedBytes = 0, 0
		var oldest time.Duration
		for _, f := range sentAt {
			oldest = max(oldest, now.Sub(f.at))
		}
		base := baseRTT.min()
		// A big frame legitimately takes a while to arrive at the target rate; only delay beyond
		// that counts as queueing.
		xmit := time.Duration(avgFrame * 8 / float64(max(bitrate, minBitrate)) * float64(time.Second))
		queueing := srtt > base+xmit+150*time.Millisecond || oldest > base+2*xmit+500*time.Millisecond
		overrun := sent > 300_000 && sent > delivered*1.25 && oldest > base+200*time.Millisecond
		if rateDebug {
			log.Printf("rate: target=%d sent=%.0f delivered=%.0f srtt=%v base=%v oldest=%v xmit=%v inflight=%d queueing=%v overrun=%v", bitrate, sent, delivered, srtt, base, oldest, xmit, len(sentAt), queueing, overrun)
		}
		switch {
		case (queueing || overrun) && now.Sub(lastDecrease) > max(500*time.Millisecond, srtt):
			// Drop to what is getting through (a bit under, so the backlog drains), then give the
			// change one round trip to show before judging again.
			target := bitrate * 7 / 10
			if delivered > float64(minBitrate) {
				target = int(delivered * 0.85)
				if oldest > base+time.Second { // a deep backlog: drain it quickly
					target = int(delivered * 0.5)
				}
			}
			everCongested = true
			bitrate = max(minBitrate, min(bitrate, target))
			lastDecrease = now
			congested = true
		case !queueing && !overrun && now.Sub(lastDecrease) > 1500*time.Millisecond && sent > float64(bitrate)*0.6:
			// Only probe upwards while we're actually using the budget (a still screen proves
			// nothing about the link).
			if everCongested {
				bitrate = min(maxRate(), bitrate*110/100)
			} else { // no limit found yet: ramp up quickly
				bitrate = min(maxRate(), bitrate*140/100)
			}
			congested = false
		case !queueing && !overrun:
			congested = false
		}
		bitrate = min(bitrate, maxRate())
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
			if r.videoErr != nil {
				videoOff = r.videoErr.Error()
				log.Printf("video encoder: %v; falling back to images", r.videoErr)
				_ = c.json(map[string]any{"t": "notice", "text": "Video isn't available on this device (" + videoOff + "); using images."})
				reset = true
			}
			stEnc += r.encode
			if r.sent {
				stFrames++
				stBytes += r.bytes
				lastActive = time.Now()
				if r.video && r.rtc {
					tokens -= float64(r.bytes)
				} else if r.video {
					sentAt[r.seq] = sentFrame{time.Now(), r.bytes}
					sentBytes += r.bytes
					tokens -= float64(r.bytes)
					avgFrame = avgFrame*0.9 + float64(r.bytes)*0.1
				} else {
					inflight++
				}
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
			case "vack":
				lastAck = time.Now()
				if f, ok := sentAt[uint32(m.I)]; ok {
					rtt := time.Since(f.at)
					// Older frames are implicitly acknowledged too (acks come in order).
					for s, o := range sentAt {
						if s <= uint32(m.I) {
							ackedBytes += o.bytes
							delete(sentAt, s)
						}
					}
					if srtt == 0 {
						srtt = rtt
					} else {
						srtt = (srtt*7 + rtt) / 8
					}
					baseRTT.add(rtt)
				}
				adjustRate()
				resume()
				continue
			case "key":
				needKey = true
			case "rtc-up", "rtc-down":
				rtcOn = m.T == "rtc-up"
				needKey = true
				clear(sentAt)
				if rtcOn {
					_ = c.json(map[string]any{"t": "rtc-on"})
				} else {
					_ = c.json(map[string]any{"t": "rtc-off"})
				}
				resume()
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
				if m.FPS >= 1 && m.FPS <= 120 {
					fps = m.FPS
				}
				if m.V >= 2 {
					proto = 2
				}
				wantVideo = m.VC && m.Mode != "images"
				if m.BW >= 0 {
					bwCap = m.BW * 1000
					if bwCap > 0 {
						bitrate = bwCap
					} else {
						bitrate = min(bitrate, 1_500_000) // start modestly, probe upwards
					}
				}
				reset = true
				needKey = true
			case "display":
				if err := scr.Select(m.I); err == nil {
					reset = true
					needKey = true
					_ = hello()
				}
			case "refresh":
				reset = true
				needKey = true
				inflight = 0
				clear(sentAt)
				resume()
			}
			expedite()

		case <-timer.C:
			video := videoOn()
			behind := queued >= len(bufs)
			if video {
				// Pace to the target bitrate whatever the encoder produces (a big keyframe or
				// a screen full of noise): wait until the budget has been earned back.
				now := time.Now()
				tokens = min(tokens+now.Sub(tokensAt).Seconds()*float64(bitrate)/8, float64(bitrate)/8/2)
				tokensAt = now
				if tokens < 0 && !needKey {
					schedule(now.Add(min(time.Duration(-tokens*8/float64(bitrate)*float64(time.Second)), 500*time.Millisecond)))
					continue
				}
				if rtcOn { // the congestion controller paces RTP; follow its estimate
					rtcMu.Lock()
					if rtcLive != nil {
						bitrate = min(maxRate(), max(minBitrate, rtcLive.Bitrate()))
					}
					rtcMu.Unlock()
				} else {
					behind = behind || len(sentAt)+queued >= videoWindow()
				}
			} else {
				behind = behind || inflight+queued >= maxInflight
			}
			if behind {
				// The viewer (or the encoder) is behind: the next ack or finished encode resumes
				// capturing. If acks stopped entirely, recover after a while.
				if time.Since(lastAck) > 5*time.Second {
					inflight = 0
					clear(sentAt)
					needKey = true
				}
				if video {
					adjustRate()
				}
				waiting = true
				schedule(time.Now().Add(time.Second))
				continue
			}
			lastCapture = time.Now()
			f, err := scr.Capture()
			stCap += time.Since(lastCapture)
			stCaptures++
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
			jobs <- encodeJob{frame: b, scale: scale, quality: quality, proto: proto, reset: reset, adaptive: proto >= 2,
				video: video, bitrate: bitrate, fps: encoderFps(sendFps, fps), key: needKey && video, at: lastCapture}
			reset = false
			if video {
				needKey = false
			}
			queued++

			if time.Since(lastStats) >= time.Second {
				secs := time.Since(lastStats).Seconds()
				sendFps = sendFps*0.5 + float64(stFrames)/secs*0.5
				mode, status := "images", ""
				if video {
					mode = "h264"
				}
				_, status = VideoCodec()
				if videoOff != "" {
					status = videoOff
				}
				avg := func(d time.Duration, n int) float64 {
					if n == 0 {
						return 0
					}
					return float64(d.Microseconds()) / float64(n) / 1000
				}
				_ = c.json(map[string]any{"t": "stats", "mode": mode, "codec": status, "fps": float64(stFrames) / secs,
					"kbps": float64(stBytes) * 8 / 1000 / secs, "target": bitrate / 1000, "rtt": srtt.Milliseconds(),
					"cap": avg(stCap, stCaptures), "enc": avg(stEnc, max(stFrames, 1)), "capture": captureMethod(scr), "congested": congested, "path": map[bool]string{true: "direct", false: "relay"}[rtcOn]})
				stCap, stEnc, stFrames, stBytes, stCaptures = 0, 0, 0, 0, 0
				lastStats = time.Now()
			}
		}
	}
}

var rateDebug = os.Getenv("APEX_RATE_DEBUG") != ""

type sentFrame struct {
	at    time.Time
	bytes int
}

// rttWindow keeps the smallest round trip seen over the last ~30 s (the path's base RTT).
type rttWindow struct {
	cur, prev time.Duration
	since     time.Time
}

func (w *rttWindow) add(rtt time.Duration) {
	if time.Since(w.since) > 15*time.Second {
		w.prev, w.cur, w.since = w.cur, 0, time.Now()
	}
	if w.cur == 0 || rtt < w.cur {
		w.cur = rtt
	}
}

func (w *rttWindow) min() time.Duration {
	switch {
	case w.prev == 0:
		return w.cur
	case w.cur == 0:
		return w.prev
	}
	return min(w.cur, w.prev)
}

// encoderFps is the frame rate the encoder budgets bits for: the rate frames actually go out
// (a slow link or CPU sends fewer than asked for, so each frame gets more bits), at least 5.
func encoderFps(measured float64, target int) int {
	if measured < 1 {
		return target
	}
	return max(5, min(target, int(measured+0.5)))
}

// captureMethod names how the screen is captured (shown in the viewer's stats).
func captureMethod(s Screen) string {
	if m, ok := s.(interface{ Method() string }); ok {
		return m.Method()
	}
	return runtime.GOOS
}

func init() {
	log.SetFlags(log.LstdFlags)
}
