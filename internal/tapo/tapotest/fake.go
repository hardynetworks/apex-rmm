// Package tapotest is a fake Tapo plug (KLAP v2 + SMART requests) for tests.
package tapotest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
)

// Plug fakes one Tapo plug. Lock-free fields are set before use; the rest go through methods.
type Plug struct {
	*httptest.Server
	mu sync.Mutex

	user, pass string
	Model      string
	Nickname   string
	Energy     bool // P110/P115: answers get_energy_usage and get_emeter_data
	Refuse     bool // TPAP firmware with Third-Party Compatibility off: 403 on handshake1

	on        bool
	powerMW   float64
	todayWh   float64
	monthWh   float64
	sessions  map[string]*sess
	Calls     []string // methods received
	Handshake int      // completed logins
}

type sess struct {
	key, iv, sig []byte
	seq          int32
	ok           bool // handshake2 done
	local        []byte
	remote       []byte
	auth         []byte
}

// New starts a fake P115 that belongs to the given TP-Link account.
func New(user, pass string) *Plug {
	p := &Plug{user: user, pass: pass, Model: "P115", Nickname: "Rack plug", Energy: true, on: true,
		powerMW: 182400, todayWh: 1234, monthWh: 45678, sessions: map[string]*sess{}}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	return p
}

// Host is the ip:port to give the client.
func (p *Plug) Host() string { return p.Listener.Addr().(*net.TCPAddr).String() }

// SetPower sets the reading in watts.
func (p *Plug) SetPower(w float64) { p.mu.Lock(); p.powerMW = w * 1000; p.mu.Unlock() }

// On reports the relay state.
func (p *Plug) On() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.on }

// SetCredentials changes the account (as if the plug were moved to another account).
func (p *Plug) SetCredentials(user, pass string) {
	p.mu.Lock()
	p.user, p.pass = user, pass
	p.mu.Unlock()
}

// SetRefuse makes handshake1 answer 403, as TPAP firmware does with Third-Party Compatibility off.
func (p *Plug) SetRefuse(b bool) { p.mu.Lock(); p.Refuse = b; p.mu.Unlock() }

// DropSessions forgets every session, as a plug does when it restarts.
func (p *Plug) DropSessions() { p.mu.Lock(); p.sessions = map[string]*sess{}; p.mu.Unlock() }

func (p *Plug) auth() []byte {
	u, w := sha1.Sum([]byte(p.user)), sha1.Sum([]byte(p.pass))
	h := sha256.Sum256(append(u[:], w[:]...))
	return h[:]
}

func h256(parts ...[]byte) []byte {
	h := sha256.New()
	for _, b := range parts {
		h.Write(b)
	}
	return h.Sum(nil)
}

func ivFor(s *sess, seq int32) []byte {
	iv := make([]byte, 16)
	copy(iv, s.iv)
	binary.BigEndian.PutUint32(iv[12:], uint32(seq))
	return iv
}

func (p *Plug) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.URL.Path {
	case "/app/handshake1":
		if p.Refuse {
			w.WriteHeader(403)
			return
		}
		if len(body) != 16 {
			w.WriteHeader(400)
			return
		}
		remote := make([]byte, 16)
		_, _ = rand.Read(remote)
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		sid := hex.EncodeToString(id)
		p.sessions[sid] = &sess{local: body, remote: remote, auth: p.auth()}
		w.Header().Add("Set-Cookie", "TP_SESSIONID="+sid+";TIMEOUT=86400")
		_, _ = w.Write(append(remote, h256(body, remote, p.auth())...))
	case "/app/handshake2":
		s := p.session(r)
		if s == nil || !bytes.Equal(body, h256(s.remote, s.local, s.auth)) {
			w.WriteHeader(403)
			return
		}
		full := h256([]byte("iv"), s.local, s.remote, s.auth)
		s.key = h256([]byte("lsk"), s.local, s.remote, s.auth)[:16]
		s.iv = full[:12]
		s.seq = int32(binary.BigEndian.Uint32(full[28:]))
		s.sig = h256([]byte("ldk"), s.local, s.remote, s.auth)[:28]
		s.ok = true
		p.Handshake++
	case "/app/request":
		s := p.session(r)
		if s == nil || !s.ok || len(body) < 48 {
			w.WriteHeader(403)
			return
		}
		n, err := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 32)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		seq := int32(n)
		var sb [4]byte
		binary.BigEndian.PutUint32(sb[:], uint32(seq))
		if !bytes.Equal(body[:32], h256(s.sig, sb[:], body[32:])) || seq <= s.seq-1000 {
			w.WriteHeader(403)
			return
		}
		block, _ := aes.NewCipher(s.key)
		plain := make([]byte, len(body)-32)
		cipher.NewCBCDecrypter(block, ivFor(s, seq)).CryptBlocks(plain, body[32:])
		plain = plain[:len(plain)-int(plain[len(plain)-1])]
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(plain, &req) != nil {
			w.WriteHeader(400)
			return
		}
		p.Calls = append(p.Calls, req.Method)
		out, _ := json.Marshal(p.handle(req.Method, req.Params))
		pad := aes.BlockSize - len(out)%aes.BlockSize
		out = append(out, bytes.Repeat([]byte{byte(pad)}, pad)...)
		ct := make([]byte, len(out))
		cipher.NewCBCEncrypter(block, ivFor(s, seq)).CryptBlocks(ct, out)
		_, _ = w.Write(append(h256(s.sig, sb[:], ct), ct...))
	default:
		w.WriteHeader(404)
	}
}

func (p *Plug) session(r *http.Request) *sess {
	c, err := r.Cookie("TP_SESSIONID")
	if err != nil {
		return nil
	}
	return p.sessions[c.Value]
}

func (p *Plug) handle(method string, params json.RawMessage) map[string]any {
	ok := func(res any) map[string]any { return map[string]any{"error_code": 0, "result": res} }
	switch method {
	case "get_device_info":
		return ok(map[string]any{"model": p.Model, "device_on": p.on, "fw_ver": "1.1.3 Build 240523 Rel.175054",
			"mac": "B0-19-21-00-00-01", "rssi": -54, "type": "SMART.TAPOPLUG",
			"nickname": base64.StdEncoding.EncodeToString([]byte(p.Nickname))})
	case "get_energy_usage":
		if !p.Energy {
			return map[string]any{"error_code": -1}
		}
		return ok(map[string]any{"current_power": p.powerMW, "today_energy": p.todayWh, "month_energy": p.monthWh})
	case "get_emeter_data":
		if !p.Energy {
			return map[string]any{"error_code": -1}
		}
		return ok(map[string]any{"power_mw": p.powerMW, "voltage_mv": 121500, "current_ma": int(p.powerMW / 121.5), "energy_wh": p.monthWh})
	case "set_device_info":
		var in struct {
			On *bool `json:"device_on"`
		}
		_ = json.Unmarshal(params, &in)
		if in.On != nil {
			p.on = *in.On
		}
		return ok(map[string]any{})
	}
	return map[string]any{"error_code": -1002}
}
