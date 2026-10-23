package agent

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// AMD Display Library (ADL) data layouts, kept free of Windows calls so they can be tested
// anywhere. ADL ships with every AMD Radeon driver on Windows (atiadlxx.dll) and is how AMD's
// own tools read GPU power, temperatures, fan and clocks; Windows has no counters for them.

const (
	adlMaxPath         = 256
	adlAdapterInfoSize = 1572 // AdapterInfo on Windows (adl_structures.h)
	adlMaxSensors      = 256  // ADL_PMLOG_MAX_SENSORS
	adlPMLogOutputSize = 4 + adlMaxSensors*8

	// ADL_PMLOG_SENSORS (adl_defines.h)
	adlClkGfx        = 1
	adlClkMem        = 2
	adlTempEdge      = 8
	adlFanPct        = 15
	adlActivityGfx   = 19
	adlASICPower     = 23
	adlTempHotspot   = 27
	adlTempGfx       = 28
	adlBoardPower    = 73
	adlFamilyNavi3   = 145  // RDNA 3 and later report the card's power as BOARD_POWER
	adlVendorAMD     = 1002 // ADL's samples compare against decimal 1002, other tools 0x1002: accept both
	adlOD6TotalPower = 0    // ODN_GPU_TOTAL_POWER for ADL2_Overdrive6_CurrentPower_Get
)

type adlAdapter struct {
	index  int
	bus    int
	device int
	fn     int
	vendor int
	name   string
	udid   string
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// parseADLAdapters reads an AdapterInfo array and keeps one entry per AMD GPU: ADL lists an
// adapter per display output, all sharing the GPU's PCI bus/device/function.
func parseADLAdapters(buf []byte) []adlAdapter {
	le := binary.LittleEndian
	var out []adlAdapter
	seen := map[[3]int]bool{}
	for off := 0; off+adlAdapterInfoSize <= len(buf); off += adlAdapterInfoSize {
		b := buf[off : off+adlAdapterInfoSize]
		a := adlAdapter{
			index:  int(int32(le.Uint32(b[4:]))),
			udid:   cstr(b[8 : 8+adlMaxPath]),
			bus:    int(int32(le.Uint32(b[264:]))),
			device: int(int32(le.Uint32(b[268:]))),
			fn:     int(int32(le.Uint32(b[272:]))),
			vendor: int(int32(le.Uint32(b[276:]))),
			name:   cstr(b[280 : 280+adlMaxPath]),
		}
		if (a.vendor != adlVendorAMD && a.vendor != 0x1002) || a.udid == "" {
			continue
		}
		k := [3]int{a.bus, a.device, a.fn}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}

// adlReading is one AMD GPU's sensors; -1 marks a value the card doesn't report.
type adlReading struct {
	Name     string  `json:"name"`
	Bus      int     `json:"bus"`
	Watts    float64 `json:"watts"`
	TempC    float64 `json:"temp_c"`
	Hotspot  float64 `json:"hotspot_c"`
	FanPct   float64 `json:"fan_pct"`
	ClockMHz float64 `json:"clock_mhz"`
	MemMHz   float64 `json:"mem_mhz"`
	Load     float64 `json:"load"`
	Source   string  `json:"source"` // "od8" (PMLog) or "od6"
}

func newADLReading(a adlAdapter) adlReading {
	return adlReading{Name: a.name, Bus: a.bus, Watts: -1, TempC: -1, Hotspot: -1, FanPct: -1, ClockMHz: -1, MemMHz: -1, Load: -1}
}

// applyPMLog fills r from an ADLPMLogDataOutput (int size; {int supported; int value}[256]).
// family is the ASIC family from ADL2_GcnAsicInfo_Get (0 if unknown).
func applyPMLog(r *adlReading, buf []byte, family int) bool {
	if len(buf) < adlPMLogOutputSize {
		return false
	}
	le := binary.LittleEndian
	get := func(i int) (float64, bool) {
		o := 4 + i*8
		if le.Uint32(buf[o:]) == 0 {
			return 0, false
		}
		return float64(int32(le.Uint32(buf[o+4:]))), true
	}
	any := false
	for i := 1; i < adlMaxSensors; i++ {
		if _, ok := get(i); ok {
			any = true
			break
		}
	}
	if !any {
		return false
	}
	r.Source = "od8"
	// RDNA 3+: ASIC_POWER isn't the card's draw there; BOARD_POWER is. Older cards: ASIC_POWER.
	order := []int{adlASICPower, adlBoardPower}
	if family >= adlFamilyNavi3 {
		order = []int{adlBoardPower, adlASICPower}
	}
	for _, i := range order {
		if v, ok := get(i); ok && v > 0 && v < 2000 {
			r.Watts = v
			break
		}
	}
	if v, ok := get(adlTempEdge); ok && v > 0 && v < 150 {
		r.TempC = v
	} else if v, ok := get(adlTempGfx); ok && v > 0 && v < 150 {
		r.TempC = v
	}
	if v, ok := get(adlTempHotspot); ok && v > 0 && v < 150 {
		r.Hotspot = v
	}
	if v, ok := get(adlFanPct); ok && v >= 0 && v <= 100 {
		r.FanPct = v
	}
	if v, ok := get(adlClkGfx); ok && v > 0 {
		r.ClockMHz = v
	}
	if v, ok := get(adlClkMem); ok && v > 0 {
		r.MemMHz = v
	}
	if v, ok := get(adlActivityGfx); ok && v >= 0 && v <= 100 {
		r.Load = v
	}
	return true
}

// matchADL pairs OS adapters (by name, in order) with ADL readings. If names don't line up but
// there is exactly one AMD adapter on each side, they are the same card.
func matchADL(names []string, readings []adlReading) []int {
	idx := make([]int, len(names))
	used := make([]bool, len(readings))
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	for i, n := range names {
		idx[i] = -1
		for j, r := range readings {
			if !used[j] && norm(r.Name) == norm(n) {
				idx[i], used[j] = j, true
				break
			}
		}
	}
	if len(names) == 1 && len(readings) == 1 && idx[0] < 0 {
		idx[0] = 0
	}
	return idx
}
