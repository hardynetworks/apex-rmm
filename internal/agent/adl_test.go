package agent

import (
	"encoding/binary"
	"testing"
)

func adlInfo(index, bus, vendor int, udid, name string) []byte {
	b := make([]byte, adlAdapterInfoSize)
	le := binary.LittleEndian
	le.PutUint32(b[0:], adlAdapterInfoSize)
	le.PutUint32(b[4:], uint32(index))
	copy(b[8:], udid)
	le.PutUint32(b[264:], uint32(bus))
	le.PutUint32(b[276:], uint32(vendor))
	copy(b[280:], name)
	copy(b[536:], `\\.\DISPLAY1`)
	return b
}

func TestParseADLAdapters(t *testing.T) {
	var buf []byte
	buf = append(buf, adlInfo(0, 3, 1002, "PCI_VEN_1002&DEV_744C", "AMD Radeon RX 7900 XTX")...)
	buf = append(buf, adlInfo(1, 3, 1002, "PCI_VEN_1002&DEV_744C", "AMD Radeon RX 7900 XTX")...) // second output, same GPU
	buf = append(buf, adlInfo(2, 12, 0x1002, "PCI_VEN_1002&DEV_164E", "AMD Radeon(TM) Graphics")...)
	buf = append(buf, adlInfo(3, 1, 0x10de, "PCI_VEN_10DE", "NVIDIA")...)
	buf = append(buf, adlInfo(4, 7, 1002, "", "AMD ghost")...)
	a := parseADLAdapters(buf)
	if len(a) != 2 || a[0].index != 0 || a[0].name != "AMD Radeon RX 7900 XTX" || a[1].index != 2 || a[1].bus != 12 {
		t.Fatalf("%+v", a)
	}
}

func pmlog(vals map[int]int32) []byte {
	b := make([]byte, adlPMLogOutputSize)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[4+i*8:], 1)
		binary.LittleEndian.PutUint32(b[4+i*8+4:], uint32(v))
	}
	return b
}

func TestApplyPMLog(t *testing.T) {
	// RDNA 3: BOARD_POWER is the card's draw
	r := newADLReading(adlAdapter{name: "AMD Radeon RX 7900 XTX"})
	ok := applyPMLog(&r, pmlog(map[int]int32{adlASICPower: 12, adlBoardPower: 287, adlTempEdge: 61, adlTempHotspot: 79,
		adlFanPct: 38, adlClkGfx: 2480, adlClkMem: 1250, adlActivityGfx: 97}), adlFamilyNavi3)
	if !ok || r.Watts != 287 || r.TempC != 61 || r.Hotspot != 79 || r.FanPct != 38 || r.ClockMHz != 2480 || r.MemMHz != 1250 || r.Load != 97 || r.Source != "od8" {
		t.Fatalf("%+v", r)
	}
	// RDNA 2: ASIC_POWER
	r = newADLReading(adlAdapter{})
	applyPMLog(&r, pmlog(map[int]int32{adlASICPower: 203, adlTempGfx: 55}), 143)
	if r.Watts != 203 || r.TempC != 55 || r.FanPct != -1 {
		t.Fatalf("%+v", r)
	}
	// RDNA 3 card that only fills ASIC_POWER
	r = newADLReading(adlAdapter{})
	applyPMLog(&r, pmlog(map[int]int32{adlASICPower: 150}), adlFamilyNavi3)
	if r.Watts != 150 {
		t.Fatalf("%+v", r)
	}
	// nothing supported
	r = newADLReading(adlAdapter{})
	if applyPMLog(&r, pmlog(nil), 0) || r.Watts != -1 {
		t.Fatalf("%+v", r)
	}
}

func TestMatchADL(t *testing.T) {
	rs := []adlReading{{Name: "AMD Radeon RX 6800"}, {Name: "AMD Radeon RX 6800"}, {Name: "AMD Radeon(TM) Graphics"}}
	m := matchADL([]string{"AMD Radeon(TM)  Graphics", "AMD Radeon RX 6800", "AMD Radeon RX 6800"}, rs)
	if m[0] != 2 || m[1] != 0 || m[2] != 1 {
		t.Fatal(m)
	}
	if m := matchADL([]string{"Radeon RX 580 Series"}, []adlReading{{Name: "AMD Radeon RX 580"}}); m[0] != 0 {
		t.Fatal("single adapter on each side should match")
	}
}
