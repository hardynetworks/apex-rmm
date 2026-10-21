package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Pure parsers for the macOS and Windows GPU readers, kept here so they are tested on every platform.

type macCard struct {
	name, vendor string
	vram         uint64
	shared       bool
}

func parseSPDisplays(out string) []macCard {
	var doc struct {
		Items []map[string]any `json:"SPDisplaysDataType"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil {
		return nil
	}
	var cards []macCard
	for _, it := range doc.Items {
		str := func(k string) string { s, _ := it[k].(string); return s }
		c := macCard{name: firstNonEmpty(str("sppci_model"), str("_name"))}
		v := strings.ToLower(str("sppci_vendor") + " " + c.name)
		switch {
		case strings.Contains(v, "apple"):
			c.vendor, c.shared = "apple", true
		case strings.Contains(v, "amd") || strings.Contains(v, "ati"):
			c.vendor = "amd"
		case strings.Contains(v, "intel"):
			c.vendor, c.shared = "intel", true
		case strings.Contains(v, "nvidia"):
			c.vendor = "nvidia"
		default:
			c.vendor = "other"
		}
		c.vram = parseSize(firstNonEmpty(str("spdisplays_vram"), str("spdisplays_vram_shared")))
		if c.name != "" {
			cards = append(cards, c)
		}
	}
	return cards
}

// parseSize reads "8 GB" / "1536 MB".
func parseSize(s string) uint64 {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(f[1]) {
	case "GB":
		return uint64(n * (1 << 30))
	case "MB":
		return uint64(n * (1 << 20))
	}
	return 0
}

var (
	reIoregEntry = regexp.MustCompile(`(?m)^\+-o `)
	reUtil       = regexp.MustCompile(`"(?:Device Utilization %|GPU Activity\(%\))"=(\d+)`)
	reMemUse     = regexp.MustCompile(`"(?:In use system memory|vramUsedBytes)"=(\d+)`)
)

type ioregAccel struct {
	load float64
	mem  uint64
}

// parseIoregAccel reads `ioreg -r -d 1 -w 0 -c IOAccelerator`, one entry per GPU.
func parseIoregAccel(out string) []ioregAccel {
	var res []ioregAccel
	idx := reIoregEntry.FindAllStringIndex(out, -1)
	for i, loc := range idx {
		end := len(out)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := out[loc[0]:end]
		a := ioregAccel{load: -1}
		if mm := reUtil.FindStringSubmatch(block); mm != nil {
			a.load, _ = strconv.ParseFloat(mm[1], 64)
		}
		if mm := reMemUse.FindStringSubmatch(block); mm != nil {
			a.mem, _ = strconv.ParseUint(mm[1], 10, 64)
		}
		if a.load >= 0 || a.mem > 0 {
			res = append(res, a)
		}
	}
	return res
}

// instance names: "pid_1234_luid_0x00000000_0x0000D1A6_phys_0_eng_0_engtype_3D"
var (
	reEngine  = regexp.MustCompile(`(?i)^pid_(\d+)_luid_(0x[0-9a-f]+_0x[0-9a-f]+)_phys_\d+_eng_\d+_engtype_(.*)$`)
	reAdMem   = regexp.MustCompile(`(?i)^luid_(0x[0-9a-f]+_0x[0-9a-f]+)_phys_\d+$`)
	reProcMem = regexp.MustCompile(`(?i)^pid_(\d+)_luid_(0x[0-9a-f]+_0x[0-9a-f]+)_phys_\d+$`)
)

// engineUse sums each engine type per adapter (and per process); the busiest engine type is the
// load, as in Task Manager.
func engineUse(vals map[string]float64) (perAdapter map[string]float64, perProc map[int32]float64) {
	type key struct{ luid, eng string }
	ad := map[key]float64{}
	type pkey struct {
		pid       int32
		luid, eng string
	}
	pr := map[pkey]float64{}
	for inst, v := range vals {
		m := reEngine.FindStringSubmatch(inst)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		luid, eng := strings.ToLower(m[2]), strings.ToLower(m[3])
		ad[key{luid, eng}] += v
		pr[pkey{int32(pid), luid, eng}] += v
	}
	perAdapter, perProc = map[string]float64{}, map[int32]float64{}
	for k, v := range ad {
		if v > perAdapter[k.luid] {
			perAdapter[k.luid] = v
		}
	}
	for k, v := range pr {
		if v > perProc[k.pid] {
			perProc[k.pid] = v
		}
	}
	return
}

func clampPct(v float64) float64 {
	if v > 100 {
		v = 100
	}
	if v < 0 {
		v = 0
	}
	return float64(int(v*10+0.5)) / 10
}
