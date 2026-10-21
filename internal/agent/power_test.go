package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func TestNUT(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch strings.TrimSpace(line) {
			case "LIST UPS":
				io.WriteString(c, "BEGIN LIST UPS\nUPS rack \"Rack UPS\"\nEND LIST UPS\n")
			case "LIST VAR rack":
				io.WriteString(c, "BEGIN LIST VAR rack\nVAR rack ups.status \"OB DISCHRG\"\nVAR rack battery.charge \"87\"\n"+
					"VAR rack battery.runtime \"1260\"\nVAR rack ups.load \"25\"\nVAR rack ups.realpower.nominal \"900\"\n"+
					"VAR rack device.mfr \"CyberPower\"\nVAR rack ups.model \"CP1500 \\\"PFC\\\"\"\nEND LIST VAR rack\n")
			default:
				io.WriteString(c, "ERR UNKNOWN-COMMAND\n")
			}
		}
	}()
	st, err := readNUT(ln.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "rack" || !st.OnBattery || st.Charge != 87 || st.RuntimeSec != 1260 || !near(st.Watts, 225) || st.Estimated || st.Model != `CyberPower CP1500 "PFC"` {
		t.Fatalf("%+v", st)
	}
}

func TestAPC(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		hdr := make([]byte, 2)
		io.ReadFull(c, hdr)
		cmd := make([]byte, binary.BigEndian.Uint16(hdr))
		io.ReadFull(c, cmd)
		for _, l := range []string{"UPSNAME  : office\n", "MODEL    : Back-UPS XS 1500G \n", "STATUS   : ONLINE \n",
			"LOADPCT  : 14.0 Percent\n", "BCHARGE  : 100.0 Percent\n", "TIMELEFT : 52.3 Minutes\n", "NOMPOWER : 865 Watts\n"} {
			binary.BigEndian.PutUint16(hdr, uint16(len(l)))
			c.Write(append(hdr, l...))
		}
		c.Write([]byte{0, 0})
	}()
	st, err := readAPC(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "office" || st.OnBattery || st.Status != "ONLINE" || !near(st.Watts, 121.1) || st.RuntimeSec != 3138 || st.NominalW != 865 {
		t.Fatalf("%+v", st)
	}
}

func TestKasa(t *testing.T) {
	if got := string(kasaCrypt(kasaCrypt([]byte(`{"a":1}`), true), false)); got != `{"a":1}` {
		t.Fatal(got)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		hdr := make([]byte, 4)
		io.ReadFull(c, hdr)
		body := make([]byte, binary.BigEndian.Uint32(hdr))
		io.ReadFull(c, body)
		if !strings.Contains(string(kasaCrypt(body, false)), "get_realtime") {
			return
		}
		resp := kasaCrypt([]byte(`{"emeter":{"get_realtime":{"power_mw":61234,"err_code":0}}}`), true)
		binary.BigEndian.PutUint32(hdr, uint32(len(resp)))
		c.Write(append(hdr, resp...))
	}()
	w, err := readKasa(ln.Addr().String())
	if err != nil || !near(w, 61.2) {
		t.Fatal(w, err)
	}
}

func TestPlugs(t *testing.T) {
	gen2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/Shelly.GetStatus" {
			io.WriteString(w, `{"sys":{},"switch:0":{"apower":42.5,"output":true},"switch:1":{"apower":7.5}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer gen2.Close()
	gen1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			io.WriteString(w, `{"meters":[{"power":88.1,"is_valid":true}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer gen1.Close()
	tas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cmnd") == "Status 10" {
			io.WriteString(w, `{"StatusSNS":{"ENERGY":{"Power":131,"Voltage":121}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer tas.Close()
	js := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"power":"12.5"}]}`)
	}))
	defer js.Close()

	p := newPowerMonitor()
	for _, c := range []struct {
		cfg  proto.EnergyConfig
		want float64
	}{
		{proto.EnergyConfig{Mode: "plug", PlugType: "shelly", PlugAddr: strings.TrimPrefix(gen2.URL, "http://")}, 50},
		{proto.EnergyConfig{Mode: "plug", PlugType: "shelly", PlugAddr: gen1.URL}, 88.1},
		{proto.EnergyConfig{Mode: "plug", PlugType: "tasmota", PlugAddr: tas.URL}, 131},
		{proto.EnergyConfig{Mode: "plug", PlugType: "json", PlugAddr: js.URL, PlugField: "data.0.power"}, 12.5},
	} {
		p.setConfig(c.cfg)
		e := p.collect(10)
		if e == nil || e.Source != "plug" || e.Kind != "measured" || !near(e.Watts, c.want) {
			t.Fatalf("%s: %+v", c.cfg.PlugType, e)
		}
	}
	// a broken plug falls back to the automatic sources, with a note
	p.setConfig(proto.EnergyConfig{Mode: "plug", PlugType: "shelly", PlugAddr: "127.0.0.1:1"})
	if e := p.collect(10); e == nil || e.Source == "plug" || !strings.HasPrefix(e.Note, "smart plug:") {
		t.Fatalf("%+v", e)
	}
}

func TestEstimateAndModes(t *testing.T) {
	p := newPowerMonitor()
	p.plat = fakePlat{}
	p.setConfig(proto.EnergyConfig{Mode: "estimate", IdleW: 40, MaxW: 140, ExtraW: 5})
	if e := p.collect(50); e.Kind != "estimate" || !near(e.Watts, 95) {
		t.Fatalf("%+v", e)
	}
	p.setConfig(proto.EnergyConfig{Mode: "off"})
	if e := p.collect(50); e != nil {
		t.Fatalf("%+v", e)
	}
	// partial: package + RAM + GPU, not the cores (inside the package), plus extra watts
	p.plat = fakePlat{ps: []proto.PowerPart{{Name: "CPU package", Watts: 20, Source: "rapl"}, {Name: "CPU cores", Watts: 15, Source: "rapl"}, {Name: "RAM", Watts: 3, Source: "rapl"}}}
	p.setConfig(proto.EnergyConfig{ExtraW: 10})
	if e := p.collect(50); e.Kind != "partial" || !near(e.Watts, 33) || len(e.Parts) != 3 {
		t.Fatalf("%+v", e)
	}
	p.plat = fakePlat{w: 180, src: "ipmi"}
	if e := p.collect(50); e.Kind != "measured" || e.Source != "ipmi" || e.Watts != 180 {
		t.Fatalf("%+v", e)
	}
	p.setConfig(proto.EnergyConfig{Mode: "ups", UPSAddr: "127.0.0.1:1"})
	if e := p.collect(50); e.Source != "ipmi" || !strings.Contains(e.Note, "no UPS") {
		t.Fatalf("%+v", e)
	}
}

type fakePlat struct {
	w   float64
	src string
	ps  []proto.PowerPart
}

func (f fakePlat) whole() (float64, string, bool) { return f.w, f.src, f.w > 0 }
func (f fakePlat) parts() []proto.PowerPart       { return append([]proto.PowerPart(nil), f.ps...) }
func (f fakePlat) onBattery() bool                { return false }

func TestJSONWatts(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"StatusSNS":{"ENERGY":{"Power":[10,20.5]}}}`), &v)
	if w, err := jsonWatts(v, "StatusSNS.ENERGY.Power"); err != nil || w != 30.5 {
		t.Fatal(w, err)
	}
	if _, err := jsonWatts(v, "nope.x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnergyCounter(t *testing.T) {
	c := &energyCounter{max: 1000_000_000}
	t0 := time.Now()
	c.watts(999_000_000, t0)
	w, ok := c.watts(29_000_000, t0.Add(2*time.Second)) // wrapped: 30 J over 2 s
	if !ok || !near(w, 15) {
		t.Fatal(w, ok)
	}
}

func TestParsers(t *testing.T) {
	if w, ok := parseDCMI("0|220,212,0,180,0,250,0"); !ok || w != 212 {
		t.Fatal(w, ok)
	}
	if w, ok := parseDCMI("0|0,220,44,1,0,0"); !ok || w != 300 {
		t.Fatal(w, ok)
	}
	if _, ok := parseDCMI("193|"); ok {
		t.Fatal("bad completion code accepted")
	}
	b := parseIOReg(`+-o AppleSmartBattery  <class AppleSmartBattery>
    {
      "ExternalConnected" = Yes
      "Voltage" = 12850
      "InstantAmperage" = 0
      "PowerTelemetryData" = {"SystemPowerIn"=18734,"SystemLoad"=12000}
    }`, true)
	if !b.external || !near(b.systemIn, 18.7) {
		t.Fatalf("%+v", b)
	}
	b = parseIOReg(`AppleSmartBattery { "ExternalConnected" = No
      "Voltage" = 12000
      "InstantAmperage" = 18446744073709550616 }`, true)
	if b.external || !near(b.discharge, 12) {
		t.Fatalf("%+v", b)
	}
	parts := parsePowermetrics("**** Processor usage ****\nCPU Power: 1234 mW\nGPU Power: 56 mW\nANE Power: 0 mW\nCombined Power (CPU + GPU + ANE): 1290 mW\n")
	if len(parts) != 3 || !near(parts[0].Watts, 1.234) || parts[2].Name != "Neural Engine" {
		t.Fatalf("%+v", parts)
	}
	parts = parsePowermetrics("Intel energy model derived package power (CPUs+GT+SA): 4.56W\n")
	if len(parts) != 1 || parts[0].Watts != 4.56 {
		t.Fatalf("%+v", parts)
	}
}
