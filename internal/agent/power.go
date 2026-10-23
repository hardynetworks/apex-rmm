package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
	"github.com/shirou/gopsutil/v4/cpu"
)

// powerMonitor works out how much power the device draws. No OS reports wall power in general,
// so it uses the best source this machine has, in this order:
//
//	plug / ups  (when chosen in the device's power settings)
//	ipmi        the server's management board (whole system, at the power supply)
//	meter       an ACPI power meter (some servers)
//	battery     a laptop running on battery, or a Mac's power telemetry (whole system)
//	rapl …      CPU package / RAM / GPU counters only ("partial")
//	estimate    idle + (max − idle) × CPU load
//
// Platform readers live in power_<os>.go.
type powerMonitor struct {
	mu  sync.Mutex
	cfg proto.EnergyConfig

	plat platformPower // OS-specific readers

	ups      upsProbe
	nvidia   cmdProbe
	virtual  bool // a VM guest: its power is part of the host's, so it isn't estimated
	plugFail int
	client   *http.Client
}

// platformPower is implemented per OS.
type platformPower interface {
	whole() (watts float64, src string, ok bool) // whole-system readings (ipmi, meter, battery…)
	parts() []proto.PowerPart                    // component readings (CPU package, RAM, GPU…)
	onBattery() bool                             // a laptop running on its battery
}

func newPowerMonitor() *powerMonitor {
	return &powerMonitor{
		cfg:    proto.EnergyConfig{Mode: "auto"},
		plat:   newPlatformPower(),
		client: &http.Client{Timeout: 4 * time.Second},
	}
}

func (p *powerMonitor) setConfig(c proto.EnergyConfig) {
	if c.Mode == "" {
		c.Mode = "auto"
	}
	p.mu.Lock()
	p.cfg = c
	p.plugFail = 0
	p.mu.Unlock()
	p.ups.reset()
}

func (p *powerMonitor) setVirtual(v bool) {
	p.mu.Lock()
	p.virtual = v
	p.mu.Unlock()
}

func (p *powerMonitor) config() proto.EnergyConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// collect returns the power reading for a metrics sample (nil when switched off).
func (p *powerMonitor) collect(cpuPct float64) *proto.Energy {
	cfg := p.config()
	if cfg.Mode == "off" {
		return nil
	}
	e := &proto.Energy{}
	e.UPS = p.ups.read(cfg.UPSAddr, cfg.UPSName)

	parts := p.plat.parts()
	if gpus := p.nvidiaParts(); len(gpus) > 0 {
		parts = append(parts, gpus...)
	}
	for i := range parts {
		parts[i].Watts = round1(parts[i].Watts)
	}
	e.Parts = parts

	switch cfg.Mode {
	case "plug":
		w, err := p.readPlug(cfg)
		if err == nil {
			p.mu.Lock()
			p.plugFail = 0
			p.mu.Unlock()
			e.Watts, e.Source, e.Kind = w, "plug", "measured"
			return finish(e)
		}
		e.Note = "smart plug: " + err.Error()
	case "ups":
		if e.UPS != nil && e.UPS.Watts > 0 {
			share := cfg.UPSShare
			if share <= 0 || share > 100 {
				share = 100
			}
			e.Watts, e.Source, e.Kind = e.UPS.Watts*share/100, "ups", "measured"
			if e.UPS.Estimated {
				e.Kind = "estimate"
			}
			return finish(e)
		}
		if e.UPS == nil {
			e.Note = "no UPS found (is NUT or apcupsd running?)"
		} else {
			e.Note = "the UPS doesn't report its load"
		}
	case "estimate":
		e.Watts, e.Source, e.Kind = p.estimate(cfg, cpuPct), "estimate", "estimate"
		return finish(e)
	}

	if w, src, ok := p.plat.whole(); ok {
		e.Watts, e.Source, e.Kind = w, src, "measured"
		return finish(e)
	}
	if len(parts) > 0 {
		sum := cfg.ExtraW
		for _, pt := range parts {
			if pt.Name != "CPU cores" && !strings.HasPrefix(pt.Name, "Integrated GPU") { // already inside the package figure
				sum += pt.Watts
			}
		}
		e.Watts, e.Source, e.Kind = sum, parts[0].Source, "partial"
		return finish(e)
	}
	p.mu.Lock()
	virtual := p.virtual
	p.mu.Unlock()
	if virtual && cfg.Mode == "auto" {
		// A virtual machine's draw is part of its host's (vSphere hosts report theirs), so an
		// estimate would count the same watts twice. "Estimate" mode still forces one.
		return nil
	}
	e.Watts, e.Source, e.Kind = p.estimate(cfg, cpuPct), "estimate", "estimate"
	return finish(e)
}

func finish(e *proto.Energy) *proto.Energy {
	if e.Watts < 0 || math.IsNaN(e.Watts) || math.IsInf(e.Watts, 0) {
		e.Watts = 0
	}
	e.Watts = round1(e.Watts)
	return e
}

// estimate: a straight line between idle and full-load draw. Without settings it guesses from
// the CPU: laptops (on a battery) idle around 8 W, desktops/servers around 30 W, plus about
// 7 W per physical core at full load.
func (p *powerMonitor) estimate(cfg proto.EnergyConfig, cpuPct float64) float64 {
	idle, max := cfg.IdleW, cfg.MaxW
	if idle <= 0 {
		idle = 30
		if hasBattery() {
			idle = 8
		}
	}
	if max <= idle {
		cores, _ := cpu.Counts(false)
		if cores <= 0 {
			cores = 4
		}
		max = idle + math.Min(7*float64(cores), 280)
	}
	return idle + (max-idle)*math.Max(0, math.Min(100, cpuPct))/100 + cfg.ExtraW
}

// ---- NVIDIA GPUs (nvidia-smi ships with the driver on Windows and Linux) ----

func (p *powerMonitor) nvidiaParts() []proto.PowerPart {
	bin := p.nvidia.find(nvidiaSMIPaths()...)
	if bin == "" {
		return nil
	}
	out, err := runCmd(5*time.Second, bin, "--query-gpu=name,power.draw", "--format=csv,noheader,nounits")
	if err != nil {
		p.nvidia.failed()
		return nil
	}
	var parts []proto.PowerPart
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 2 {
			continue
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(f[len(f)-1]), 64)
		if err != nil { // "[N/A]" on cards without a power sensor
			continue
		}
		parts = append(parts, proto.PowerPart{Name: strings.TrimSpace(strings.Join(f[:len(f)-1], ",")), Watts: w, Source: "nvidia"})
	}
	return parts
}

// cmdProbe remembers whether a tool exists, re-checking now and then.
type cmdProbe struct {
	mu      sync.Mutex
	path    string
	checked time.Time
}

func (c *cmdProbe) find(candidates ...string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "" || time.Since(c.checked) < time.Hour {
		return c.path
	}
	c.checked = time.Now()
	for _, cand := range candidates {
		if p, err := exec.LookPath(cand); err == nil {
			c.path = p
			break
		}
	}
	return c.path
}

func (c *cmdProbe) failed() {
	c.mu.Lock()
	c.path, c.checked = "", time.Now()
	c.mu.Unlock()
}

// ---- smart plugs ----

func (p *powerMonitor) readPlug(cfg proto.EnergyConfig) (float64, error) {
	addr := strings.TrimSpace(cfg.PlugAddr)
	if addr == "" {
		return 0, errors.New("no address set")
	}
	switch cfg.PlugType {
	case "shelly":
		return p.readShelly(addr)
	case "tasmota":
		var v map[string]any
		if err := p.getJSON(plugURL(addr, "/cm?cmnd=Status%2010"), &v); err != nil {
			return 0, err
		}
		return jsonWatts(v, "StatusSNS.ENERGY.Power")
	case "kasa":
		return readKasa(addr)
	case "json":
		var v any
		if err := p.getJSON(plugURL(addr, ""), &v); err != nil {
			return 0, err
		}
		field := cfg.PlugField
		if field == "" {
			field = "power"
		}
		return jsonWatts(v, field)
	}
	return 0, fmt.Errorf("unknown plug type %q", cfg.PlugType)
}

func plugURL(addr, path string) string {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	if path == "" {
		return addr
	}
	return strings.TrimRight(addr, "/") + path
}

func (p *powerMonitor) getJSON(url string, v any) error {
	resp, err := p.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// Shelly: Gen2+ (Plus/Pro/Gen3/Gen4) answer Shelly.GetStatus over RPC with "switch:N",
// "pm1:N" or "em:N" components; Gen1 devices answer /status with meters/emeters.
func (p *powerMonitor) readShelly(addr string) (float64, error) {
	var st map[string]any
	if err := p.getJSON(plugURL(addr, "/rpc/Shelly.GetStatus"), &st); err == nil {
		sum, found := 0.0, false
		for k, v := range st {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			switch {
			case strings.HasPrefix(k, "switch:"), strings.HasPrefix(k, "pm1:"), strings.HasPrefix(k, "cover:"):
				if w, ok := m["apower"].(float64); ok {
					sum, found = sum+w, true
				}
			case strings.HasPrefix(k, "em:"):
				if w, ok := m["total_act_power"].(float64); ok {
					sum, found = sum+w, true
				}
			case strings.HasPrefix(k, "em1:"):
				if w, ok := m["act_power"].(float64); ok {
					sum, found = sum+w, true
				}
			}
		}
		if found {
			return sum, nil
		}
	}
	var g1 struct {
		Meters  []struct{ Power float64 } `json:"meters"`
		EMeters []struct{ Power float64 } `json:"emeters"`
	}
	if err := p.getJSON(plugURL(addr, "/status"), &g1); err != nil {
		return 0, err
	}
	if len(g1.Meters)+len(g1.EMeters) == 0 {
		return 0, errors.New("the Shelly device doesn't report power")
	}
	sum := 0.0
	for _, m := range g1.Meters {
		sum += m.Power
	}
	for _, m := range g1.EMeters {
		sum += m.Power
	}
	return sum, nil
}

// jsonWatts follows a dotted path ("StatusSNS.ENERGY.Power", "data.0.power") to a number.
func jsonWatts(v any, path string) (float64, error) {
	cur := v
	for _, key := range strings.Split(path, ".") {
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[key]
			if !ok {
				return 0, fmt.Errorf("no %q in the reply", path)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(c) {
				return 0, fmt.Errorf("no %q in the reply", path)
			}
			cur = c[i]
		default:
			return 0, fmt.Errorf("no %q in the reply", path)
		}
	}
	switch x := cur.(type) {
	case float64:
		return x, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	case []any: // Tasmota reports an array for multi-channel devices
		sum := 0.0
		for _, e := range x {
			if f, ok := e.(float64); ok {
				sum += f
			}
		}
		return sum, nil
	}
	return 0, fmt.Errorf("%q isn't a number", path)
}

// Kasa plugs with energy monitoring (HS110, KP115, KP125, EP25…) on the local protocol: JSON
// "encrypted" with an XOR autokey, length-prefixed, on TCP 9999. Plugs on newer firmware that
// only speak the authenticated KLAP protocol aren't supported.
func readKasa(addr string) (float64, error) {
	if !strings.Contains(addr, ":") {
		addr += ":9999"
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	req := kasaCrypt([]byte(`{"emeter":{"get_realtime":{}}}`), true)
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(req)))
	if _, err := conn.Write(append(hdr, req...)); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, errors.New("no answer (newer Kasa firmware isn't supported)")
	}
	n := binary.BigEndian.Uint32(hdr)
	if n > 1<<16 {
		return 0, errors.New("bad reply")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, err
	}
	var r struct {
		Emeter struct {
			Realtime struct {
				Power   *float64 `json:"power"`
				PowerMW *float64 `json:"power_mw"`
				ErrCode int      `json:"err_code"`
			} `json:"get_realtime"`
		} `json:"emeter"`
	}
	if err := json.Unmarshal(kasaCrypt(body, false), &r); err != nil {
		return 0, err
	}
	rt := r.Emeter.Realtime
	switch {
	case rt.PowerMW != nil:
		return *rt.PowerMW / 1000, nil
	case rt.Power != nil:
		return *rt.Power, nil
	}
	return 0, errors.New("the plug doesn't measure power")
}

func kasaCrypt(b []byte, encrypt bool) []byte {
	key := byte(171)
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = key ^ c
		if encrypt {
			key = out[i]
		} else {
			key = c
		}
	}
	return out
}

// ---- UPS: NUT (port 3493) or apcupsd (port 3551) ----

type upsProbe struct {
	mu       sync.Mutex
	kind     string // "nut", "apc" or "" (none found)
	nextScan time.Time
}

func (u *upsProbe) reset() {
	u.mu.Lock()
	u.kind, u.nextScan = "", time.Time{}
	u.mu.Unlock()
}

func (u *upsProbe) read(addr, name string) *proto.UPSStatus {
	u.mu.Lock()
	kind, next := u.kind, u.nextScan
	u.mu.Unlock()
	host := strings.TrimSpace(addr)
	if host == "" {
		host = "127.0.0.1"
	}
	try := func(k string) *proto.UPSStatus {
		var st *proto.UPSStatus
		var err error
		if k == "nut" {
			st, err = readNUT(withPort(host, "3493"), name)
		} else {
			st, err = readAPC(withPort(host, "3551"))
		}
		if err != nil {
			return nil
		}
		return st
	}
	if kind != "" {
		if st := try(kind); st != nil {
			return st
		}
	}
	if time.Now().Before(next) {
		return nil
	}
	for _, k := range []string{"nut", "apc"} {
		if st := try(k); st != nil {
			u.mu.Lock()
			u.kind = k
			u.mu.Unlock()
			return st
		}
	}
	u.mu.Lock()
	u.kind, u.nextScan = "", time.Now().Add(10*time.Minute)
	u.mu.Unlock()
	return nil
}

func withPort(host, port string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

func readNUT(addr, name string) (*proto.UPSStatus, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	r := bufio.NewReader(conn)
	list := func(cmd, prefix string) ([][]string, error) {
		if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
			return nil, err
		}
		var rows [][]string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return nil, err
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ERR") {
				return nil, errors.New(line)
			}
			if strings.HasPrefix(line, "END LIST") {
				return rows, nil
			}
			if strings.HasPrefix(line, prefix+" ") {
				rows = append(rows, nutFields(line))
			}
		}
	}
	if name == "" {
		ups, err := list("LIST UPS", "UPS")
		if err != nil || len(ups) == 0 || len(ups[0]) < 2 {
			return nil, errors.New("no UPS")
		}
		name = ups[0][1]
	}
	vars, err := list("LIST VAR "+name, "VAR")
	if err != nil {
		return nil, err
	}
	v := map[string]string{}
	for _, f := range vars {
		if len(f) >= 4 {
			v[f[2]] = f[3]
		}
	}
	num := func(k string) float64 {
		f, err := strconv.ParseFloat(v[k], 64)
		if err != nil {
			return -1
		}
		return f
	}
	st := &proto.UPSStatus{Name: name, Model: strings.TrimSpace(v["device.mfr"] + " " + firstSet(v["ups.model"], v["device.model"])),
		Status: v["ups.status"], Charge: num("battery.charge"), LoadPct: num("ups.load")}
	st.OnBattery = strings.Contains(" "+st.Status+" ", " OB ")
	st.RuntimeSec = int(num("battery.runtime"))
	if p := num("ups.realpower"); p > 0 {
		st.Watts = p
	}
	st.NominalW = math.Max(0, num("ups.realpower.nominal"))
	if st.Watts == 0 && st.LoadPct >= 0 {
		if st.NominalW > 0 {
			st.Watts = st.LoadPct / 100 * st.NominalW
		} else if va := num("ups.power.nominal"); va > 0 {
			st.Watts, st.Estimated = st.LoadPct/100*va*0.6, true // typical consumer UPS: W ≈ 0.6 × VA
		}
	}
	st.Watts = round1(st.Watts)
	return st, nil
}

// nutFields splits a NUT protocol line, honouring double quotes.
func nutFields(line string) []string {
	var out []string
	var cur strings.Builder
	inQ, esc := false, false
	for _, c := range line {
		switch {
		case esc:
			cur.WriteRune(c)
			esc = false
		case c == '\\' && inQ:
			esc = true
		case c == '"':
			inQ = !inQ
		case c == ' ' && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 || inQ {
		out = append(out, cur.String())
	}
	return out
}

func readAPC(addr string) (*proto.UPSStatus, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := conn.Write([]byte{0, 6, 's', 't', 'a', 't', 'u', 's'}); err != nil {
		return nil, err
	}
	v := map[string]string{}
	hdr := make([]byte, 2)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint16(hdr)
		if n == 0 {
			break
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		if k, val, ok := strings.Cut(string(b), ":"); ok {
			v[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	if len(v) == 0 {
		return nil, errors.New("no status")
	}
	num := func(k string) float64 {
		f := strings.Fields(v[k])
		if len(f) == 0 {
			return -1
		}
		x, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			return -1
		}
		return x
	}
	st := &proto.UPSStatus{Name: firstSet(v["UPSNAME"], "apcupsd"), Model: v["MODEL"], Status: v["STATUS"],
		Charge: num("BCHARGE"), LoadPct: num("LOADPCT")}
	st.OnBattery = strings.Contains(st.Status, "ONBATT")
	if m := num("TIMELEFT"); m >= 0 {
		st.RuntimeSec = int(m * 60)
	} else {
		st.RuntimeSec = -1
	}
	st.NominalW = math.Max(0, num("NOMPOWER"))
	if st.LoadPct >= 0 && st.NominalW > 0 {
		st.Watts = round1(st.LoadPct / 100 * st.NominalW)
	} else if va := num("NOMAPNT"); st.LoadPct >= 0 && va > 0 {
		st.Watts, st.Estimated = round1(st.LoadPct/100*va*0.6), true
	}
	return st, nil
}

func firstSet(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// energyCounter turns a cumulative energy counter (µJ, with wrap-around) into watts.
type energyCounter struct {
	last uint64
	at   time.Time
	max  uint64
}

func (e *energyCounter) watts(v uint64, now time.Time) (float64, bool) {
	defer func() { e.last, e.at = v, now }()
	if e.at.IsZero() {
		return 0, false
	}
	dt := now.Sub(e.at).Seconds()
	if dt <= 0.05 {
		return 0, false
	}
	d := v - e.last
	if v < e.last {
		if e.max == 0 {
			return 0, false
		}
		d = e.max - e.last + v
	}
	return float64(d) / 1e6 / dt, true
}
