// Package tapo reads and switches TP-Link Tapo Wi-Fi smart plugs (P100, P110, P115, P125…) on the
// local network, without the TP-Link cloud.
//
// The plugs speak KLAP: a two-step handshake proves both sides know the TP-Link account's
// email and password (they are never sent), then every request is AES-128-CBC encrypted with
// keys derived from the handshake. Inside that, requests are Tapo "SMART" JSON calls such as
// get_device_info and get_energy_usage.
//
// Plugs with firmware 1.4 or newer only accept KLAP while "Third-Party Compatibility" is on in
// the Tapo app; with it off they use TP-Link's undocumented TPAP login, which isn't supported.
package tapo

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrAuth: the plug answered, but not to the email and password Apex has.
var ErrAuth = errors.New("the plug didn't accept the TP-Link email and password (both are case-sensitive)")

// ErrRefused: the plug turned the KLAP login away, which newer firmware does while
// "Third-Party Compatibility" is off in the Tapo app.
var ErrRefused = errors.New(`the plug refused the local login: in the Tapo app open Me → Third-Party Services and turn on "Third-Party Compatibility"`)

// DeviceError is an error code returned by the plug for a request.
type DeviceError struct {
	Method string
	Code   int
}

func (e *DeviceError) Error() string {
	return fmt.Sprintf("%s: the plug returned error %d", e.Method, e.Code)
}

// Credentials are the TP-Link account the plug was set up with.
type Credentials struct {
	Username string // the TP-Link ID (email)
	Password string
}

// Auth hashes. KLAP v2 (Tapo) uses SHA-256(SHA-1(user) || SHA-1(pass)); v1 (older Kasa KLAP)
// uses MD5(MD5(user) || MD5(pass)).
func authHashV2(c Credentials) []byte {
	u, p := sha1.Sum([]byte(c.Username)), sha1.Sum([]byte(c.Password))
	h := sha256.Sum256(append(u[:], p[:]...))
	return h[:]
}

func authHashV1(c Credentials) []byte {
	u, p := md5.Sum([]byte(c.Username)), md5.Sum([]byte(c.Password))
	h := md5.Sum(append(u[:], p[:]...))
	return h[:]
}

func sum256(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// candidate is one way of hashing credentials the plug might be expecting.
type candidate struct {
	v2   bool
	hash []byte
}

// Plugs that were never linked to the TP-Link cloud take blank credentials, and some linked
// ones intermittently answer to a factory default account (as python-kasa found).
var defaultTapo = Credentials{Username: "test@tp-link.net", Password: "test"}

func candidates(c Credentials) []candidate {
	out := []candidate{{true, authHashV2(c)}, {false, authHashV1(c)}}
	for _, d := range []Credentials{defaultTapo, {}} {
		if d != c {
			out = append(out, candidate{true, authHashV2(d)}, candidate{false, authHashV1(d)})
		}
	}
	return out
}

func hs1Hash(v2 bool, local, remote, auth []byte) []byte {
	if v2 {
		return sum256(local, remote, auth)
	}
	return sum256(local, auth)
}

func hs2Hash(v2 bool, local, remote, auth []byte) []byte {
	if v2 {
		return sum256(remote, local, auth)
	}
	return sum256(remote, auth)
}

// session is an established KLAP session: key, IV prefix, signature key and sequence number.
type session struct {
	key     []byte // 16
	iv      []byte // 12
	sig     []byte // 28
	seq     int32
	cookie  string
	expires time.Time
}

func newSession(local, remote, auth []byte) *session {
	full := sum256([]byte("iv"), local, remote, auth)
	return &session{
		key: sum256([]byte("lsk"), local, remote, auth)[:16],
		iv:  full[:12],
		seq: int32(binary.BigEndian.Uint32(full[28:])),
		sig: sum256([]byte("ldk"), local, remote, auth)[:28],
	}
}

func (s *session) ivFor(seq int32) []byte {
	iv := make([]byte, 16)
	copy(iv, s.iv)
	binary.BigEndian.PutUint32(iv[12:], uint32(seq))
	return iv
}

// encrypt: next sequence number, then SHA-256(sig || seq || ciphertext) || ciphertext.
func (s *session) encrypt(msg []byte) ([]byte, int32) {
	s.seq++
	seq := s.seq
	pad := aes.BlockSize - len(msg)%aes.BlockSize
	plain := append(append([]byte{}, msg...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(s.key)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, s.ivFor(seq)).CryptBlocks(ct, plain)
	var sb [4]byte
	binary.BigEndian.PutUint32(sb[:], uint32(seq))
	return append(sum256(s.sig, sb[:], ct), ct...), seq
}

func (s *session) decrypt(seq int32, msg []byte) ([]byte, error) {
	if len(msg) < 32+aes.BlockSize || (len(msg)-32)%aes.BlockSize != 0 {
		return nil, errors.New("short or misaligned response")
	}
	ct := msg[32:]
	block, _ := aes.NewCipher(s.key)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, s.ivFor(seq)).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("bad padding (wrong key?)")
	}
	return plain[:len(plain)-pad], nil
}

// Client talks to one plug. It is safe for concurrent use; requests are serialised because
// the plug expects the sequence numbers in order.
type Client struct {
	host  string // ip or ip:port
	creds Credentials
	hc    *http.Client
	uuid  string

	mu   sync.Mutex
	base string // "http://ip:80" or "https://ip:4433" once known
	sess *session
}

// New makes a client for the plug at host (an IP address or name, optionally with :port).
func New(host string, c Credentials) *Client {
	u := make([]byte, 16)
	_, _ = rand.Read(u)
	m := md5.Sum(u)
	return &Client{
		host:  strings.TrimSpace(host),
		creds: c,
		uuid:  base64.StdEncoding.EncodeToString(m[:]),
		hc: &http.Client{
			Timeout: 8 * time.Second,
			// Newer plugs can serve KLAP over HTTPS on 4433 with a self-signed certificate;
			// the KLAP handshake itself authenticates the plug.
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
				DialContext:         (&net.Dialer{Timeout: 4 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

// Host is the address the client was made for.
func (c *Client) Host() string { return c.host }

// Close drops idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

func (c *Client) bases() []string {
	if c.base != "" {
		return []string{c.base}
	}
	if _, _, err := net.SplitHostPort(c.host); err == nil {
		return []string{"http://" + c.host}
	}
	return []string{"http://" + net.JoinHostPort(c.host, "80"), "https://" + net.JoinHostPort(c.host, "4433")}
}

func (c *Client) post(ctx context.Context, url string, body []byte, cookie string) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, resp.Header, err
}

// handshake runs KLAP handshake1 + handshake2. Call with c.mu held.
func (c *Client) handshake(ctx context.Context) error {
	c.sess = nil
	local := make([]byte, 16)
	_, _ = rand.Read(local)
	var lastErr error
	for _, base := range c.bases() {
		status, body, hdr, err := c.post(ctx, base+"/app/handshake1", local, "")
		if err != nil {
			lastErr = err
			continue
		}
		if status != 200 {
			// 403/404 here: the plug has KLAP switched off (TPAP) or is not a KLAP plug
			if status == 403 || status == 401 || status == 404 {
				return ErrRefused
			}
			return fmt.Errorf("handshake1: HTTP %d", status)
		}
		if len(body) != 48 {
			return fmt.Errorf("handshake1: unexpected %d-byte answer (not a KLAP plug?)", len(body))
		}
		remote, server := body[:16], body[16:]
		var hit *candidate
		for _, cand := range candidates(c.creds) {
			if subtle.ConstantTimeCompare(hs1Hash(cand.v2, local, remote, cand.hash), server) == 1 {
				cc := cand
				hit = &cc
				break
			}
		}
		if hit == nil {
			return ErrAuth
		}
		cookie, timeout := sessionCookie(hdr)
		status, _, _, err = c.post(ctx, base+"/app/handshake2", hs2Hash(hit.v2, local, remote, hit.hash), cookie)
		if err != nil {
			return fmt.Errorf("handshake2: %w", err)
		}
		if status != 200 {
			return fmt.Errorf("handshake2: HTTP %d", status)
		}
		s := newSession(local, remote, hit.hash)
		s.cookie = cookie
		s.expires = time.Now().Add(timeout - 20*time.Minute)
		c.sess, c.base = s, base
		return nil
	}
	return fmt.Errorf("can't reach the plug at %s: %w", c.host, lastErr)
}

// sessionCookie keeps TP_SESSIONID and reads TIMEOUT (seconds, default a day).
func sessionCookie(h http.Header) (string, time.Duration) {
	timeout := 24 * time.Hour
	var keep []string
	for _, line := range h.Values("Set-Cookie") {
		for _, part := range strings.Split(line, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			switch k {
			case "TP_SESSIONID":
				keep = append(keep, k+"="+v)
			case "TIMEOUT":
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					timeout = time.Duration(n) * time.Second
				}
			}
		}
	}
	if timeout < 30*time.Minute {
		timeout = 30 * time.Minute
	}
	return strings.Join(keep, "; "), timeout
}

// Call sends one SMART request and returns its "result". It logs in when needed and once more
// if the plug drops the session.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	req, _ := json.Marshal(map[string]any{
		"method": method, "params": params,
		"request_time_milis": time.Now().UnixMilli(), "terminal_uuid": c.uuid,
	})
	for attempt := 0; ; attempt++ {
		if c.sess == nil || time.Now().After(c.sess.expires) {
			if err := c.handshake(ctx); err != nil {
				return nil, err
			}
		}
		payload, seq := c.sess.encrypt(req)
		status, body, _, err := c.post(ctx, fmt.Sprintf("%s/app/request?seq=%d", c.base, seq), payload, c.sess.cookie)
		if err == nil && status == 200 {
			plain, derr := c.sess.decrypt(seq, body)
			if derr != nil {
				err = fmt.Errorf("decrypt: %w", derr)
			} else {
				var out struct {
					Code   int             `json:"error_code"`
					Result json.RawMessage `json:"result"`
				}
				if jerr := json.Unmarshal(plain, &out); jerr != nil {
					return nil, fmt.Errorf("%s: bad JSON from the plug: %w", method, jerr)
				}
				if out.Code != 0 {
					return nil, &DeviceError{Method: method, Code: out.Code}
				}
				return out.Result, nil
			}
		} else if err == nil {
			err = fmt.Errorf("%s: HTTP %d", method, status)
		}
		// the session expired, the plug restarted, or the connection dropped: log in again once
		c.sess = nil
		if attempt > 0 || ctx.Err() != nil {
			return nil, err
		}
	}
}

// State is what Apex reads from a plug on every poll.
type State struct {
	Model    string   `json:"model"`
	Nickname string   `json:"nickname"`
	Firmware string   `json:"firmware"`
	MAC      string   `json:"mac"`
	RSSI     int      `json:"rssi"`
	On       bool     `json:"on"`
	Watts    *float64 `json:"watts"` // nil for plugs without energy monitoring (P100, P105)
	Volts    *float64 `json:"volts"`
	Amps     *float64 `json:"amps"`
	TodayKWh *float64 `json:"today_kwh"`
	MonthKWh *float64 `json:"month_kwh"`
}

func f64(v float64) *float64 { return &v }

// Read gets the plug's info and, when it measures power, its energy readings.
func (c *Client) Read(ctx context.Context) (State, error) {
	var st State
	raw, err := c.Call(ctx, "get_device_info", nil)
	if err != nil {
		return st, err
	}
	var info struct {
		Model    string `json:"model"`
		Nickname string `json:"nickname"`
		FW       string `json:"fw_ver"`
		MAC      string `json:"mac"`
		RSSI     int    `json:"rssi"`
		On       bool   `json:"device_on"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return st, fmt.Errorf("get_device_info: %w", err)
	}
	st.Model, st.Firmware, st.MAC, st.RSSI, st.On = info.Model, info.FW, info.MAC, info.RSSI, info.On
	st.Nickname = info.Nickname
	if b, err := base64.StdEncoding.DecodeString(info.Nickname); err == nil {
		st.Nickname = string(b)
	}

	raw, err = c.Call(ctx, "get_energy_usage", nil)
	var de *DeviceError
	if errors.As(err, &de) {
		return st, nil // no energy monitoring on this model
	}
	if err != nil {
		return st, err
	}
	var e struct {
		CurrentPower *float64 `json:"current_power"` // mW
		Today        *float64 `json:"today_energy"`  // Wh
		Month        *float64 `json:"month_energy"`  // Wh
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return st, fmt.Errorf("get_energy_usage: %w", err)
	}
	if e.CurrentPower != nil {
		st.Watts = f64(*e.CurrentPower / 1000)
	}
	if e.Today != nil {
		st.TodayKWh = f64(*e.Today / 1000)
	}
	if e.Month != nil {
		st.MonthKWh = f64(*e.Month / 1000)
	}
	// voltage, current and a finer power reading where the firmware has them
	if raw, err := c.Call(ctx, "get_emeter_data", nil); err == nil {
		var m struct {
			PowerMW   *float64 `json:"power_mw"`
			VoltageMV *float64 `json:"voltage_mv"`
			CurrentMA *float64 `json:"current_ma"`
		}
		if json.Unmarshal(raw, &m) == nil {
			if m.PowerMW != nil {
				st.Watts = f64(*m.PowerMW / 1000)
			}
			if m.VoltageMV != nil {
				st.Volts = f64(*m.VoltageMV / 1000)
			}
			if m.CurrentMA != nil {
				st.Amps = f64(*m.CurrentMA / 1000)
			}
		}
	} else if ctx.Err() != nil {
		return st, ctx.Err()
	}
	if st.Watts == nil {
		if raw, err := c.Call(ctx, "get_current_power", nil); err == nil {
			var p struct {
				W *float64 `json:"current_power"` // W
			}
			if json.Unmarshal(raw, &p) == nil && p.W != nil {
				st.Watts = f64(*p.W)
			}
		}
	}
	return st, nil
}

// SetOn switches the plug's relay.
func (c *Client) SetOn(ctx context.Context, on bool) error {
	_, err := c.Call(ctx, "set_device_info", map[string]any{"device_on": on})
	return err
}
