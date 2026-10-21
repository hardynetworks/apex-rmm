//go:build linux

package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// Linux GPUs come from sysfs (/sys/class/drm/cardN/device):
//   - AMD (amdgpu): gpu_busy_percent, mem_info_vram_*, hwmon temperature / fan / power / clocks
//   - Intel (i915, xe): current clock; load from the DRM clients' engine time (fdinfo)
//   - NVIDIA without nvidia-smi: name only
// Per-process use comes from /proc/<pid>/fdinfo, which amdgpu, i915, xe and other DRM drivers
// fill with each client's engine time and memory.

type linuxGPU struct {
	mu       sync.Mutex
	nameMemo map[string]string
	busy     map[string]clientSample // fdinfo totals per card, for Intel load between samples
}

type clientSample struct {
	at    time.Time
	busy  map[string]uint64 // engine -> ns, summed over clients
	cyc   map[string]uint64 // xe: engine -> cycles
	total map[string]uint64 // xe: engine -> total cycles
}

func newPlatformGPU() platformGPU {
	return &linuxGPU{nameMemo: map[string]string{}, busy: map[string]clientSample{}}
}

// Display adapters that are not real GPUs: virtual machines' adapters and servers' BMC graphics.
var nonGPUVendors = map[string]bool{
	"0x15ad": true, // VMware SVGA
	"0x1234": true, // QEMU/Bochs
	"0x1af4": true, // virtio
	"0x1b36": true, // QXL
	"0x1414": true, // Hyper-V
	"0x80ee": true, // VirtualBox
	"0x1a03": true, // ASPEED (BMC)
	"0x102b": true, // Matrox G200 (server BMC)
	"0x19a2": true, // Emulex BMC graphics
}

var cardRe = regexp.MustCompile(`^card\d+$`)

type sysCard struct {
	card, dev, vendor, device, slot, driver string
}

func (l *linuxGPU) cards() []sysCard {
	ents, _ := os.ReadDir(filepath.Join(sysRoot, "class/drm"))
	seen := map[string]bool{}
	var out []sysCard
	for _, e := range ents {
		if !cardRe.MatchString(e.Name()) {
			continue
		}
		card := filepath.Join(sysRoot, "class/drm", e.Name())
		dev := filepath.Join(card, "device")
		vendor := readSys(filepath.Join(dev, "vendor"))
		if vendor == "" || nonGPUVendors[vendor] {
			continue
		}
		real, _ := filepath.EvalSymlinks(dev)
		slot := filepath.Base(real)
		if seen[slot] {
			continue
		}
		seen[slot] = true
		drv, _ := os.Readlink(filepath.Join(dev, "driver"))
		out = append(out, sysCard{card: card, dev: dev, vendor: vendor, device: readSys(filepath.Join(dev, "device")), slot: slot, driver: filepath.Base(drv)})
	}
	return out
}

func readSys(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readSysNum(p string) (float64, bool) {
	v, err := strconv.ParseFloat(readSys(p), 64)
	return v, err == nil
}

func vendorName(v string) string {
	switch v {
	case "0x1002":
		return "amd"
	case "0x8086":
		return "intel"
	case "0x10de":
		return "nvidia"
	}
	return "other"
}

func (l *linuxGPU) names() []string {
	var out []string
	for _, c := range l.cards() {
		out = append(out, l.name(c))
	}
	return out
}

func (l *linuxGPU) gpus(skipNVIDIA bool) []proto.GPU {
	l.mu.Lock()
	defer l.mu.Unlock()
	cards := l.cards()
	if len(cards) == 0 {
		return nil
	}
	var intelCards []sysCard
	var out []proto.GPU
	for _, c := range cards {
		vend := vendorName(c.vendor)
		if vend == "nvidia" && skipNVIDIA {
			continue
		}
		g := proto.GPU{Name: l.name(c), Vendor: vend, Load: -1, TempC: -1, FanPct: -1, Watts: -1, Source: "sysfs"}
		if v, ok := readSysNum(filepath.Join(c.dev, "gpu_busy_percent")); ok {
			g.Load = v
		}
		if v, ok := readSysNum(filepath.Join(c.dev, "mem_info_vram_total")); ok && v > 0 {
			g.MemTotal = uint64(v)
			if u, ok := readSysNum(filepath.Join(c.dev, "mem_info_vram_used")); ok {
				g.MemUsed = uint64(u)
			}
		}
		l.hwmon(c, &g)
		if vend == "intel" {
			g.Shared = true
			for _, p := range []string{filepath.Join(c.card, "gt_cur_freq_mhz"), filepath.Join(c.dev, "tile0/gt0/freq0/cur_freq")} {
				if v, ok := readSysNum(p); ok && v > 0 {
					g.ClockMHz = v
					break
				}
			}
			intelCards = append(intelCards, c)
		}
		out = append(out, g)
	}
	// Intel has no busy counter in sysfs: work the load out from the DRM clients' engine time
	// since the previous sample.
	if len(intelCards) > 0 {
		now := time.Now()
		tot := drmTotals(scanFdinfo(""))
		for i := range out {
			if out[i].Vendor != "intel" || out[i].Load >= 0 {
				continue
			}
			c := cardByName(intelCards, out[i].Name, l)
			if c == nil {
				continue
			}
			cur := tot[c.slot]
			cur.at = now
			if prev, ok := l.busy[c.slot]; ok {
				out[i].Load = engineLoad(prev, cur)
			}
			l.busy[c.slot] = cur
		}
	}
	return out
}

func cardByName(cards []sysCard, name string, l *linuxGPU) *sysCard {
	for i := range cards {
		if l.name(cards[i]) == name {
			return &cards[i]
		}
	}
	return nil
}

// hwmon reads the card's sensors: temperature (m°C), fan (PWM or RPM vs max), power (µW), clocks (Hz).
func (l *linuxGPU) hwmon(c sysCard, g *proto.GPU) {
	dirs, _ := filepath.Glob(filepath.Join(c.dev, "hwmon", "hwmon*"))
	for _, d := range dirs {
		if v, ok := readSysNum(filepath.Join(d, "temp1_input")); ok {
			g.TempC = v / 1000
		}
		if v, ok := readSysNum(filepath.Join(d, "pwm1")); ok {
			g.FanPct = v / 255 * 100
		} else if rpm, ok := readSysNum(filepath.Join(d, "fan1_input")); ok {
			if max, ok := readSysNum(filepath.Join(d, "fan1_max")); ok && max > 0 {
				g.FanPct = rpm / max * 100
			}
		}
		for _, f := range []string{"power1_average", "power1_input"} {
			if v, ok := readSysNum(filepath.Join(d, f)); ok && v > 0 {
				g.Watts = v / 1e6
				break
			}
		}
		if v, ok := readSysNum(filepath.Join(d, "freq1_input")); ok && v > 0 {
			g.ClockMHz = v / 1e6
		}
		if v, ok := readSysNum(filepath.Join(d, "freq2_input")); ok && v > 0 {
			g.MemClock = v / 1e6
		}
	}
	if g.TempC >= 0 {
		g.TempC = float64(int(g.TempC*10+0.5)) / 10
	}
	if g.FanPct >= 0 {
		g.FanPct = float64(int(g.FanPct + 0.5))
	}
}

// name looks the card up in the PCI ID database, falling back to the vendor and device id.
func (l *linuxGPU) name(c sysCard) string {
	key := c.vendor + ":" + c.device
	if n, ok := l.nameMemo[key]; ok {
		return n
	}
	n := strings.TrimSpace(readSys(filepath.Join(c.dev, "product_name")))
	if n == "" {
		n = pciName(strings.TrimPrefix(c.vendor, "0x"), strings.TrimPrefix(c.device, "0x"))
	}
	if n == "" {
		n = map[string]string{"amd": "AMD GPU", "intel": "Intel GPU", "nvidia": "NVIDIA GPU"}[vendorName(c.vendor)]
		if n == "" {
			n = "GPU"
		}
		n += fmt.Sprintf(" (%s:%s)", strings.TrimPrefix(c.vendor, "0x"), strings.TrimPrefix(c.device, "0x"))
	}
	l.nameMemo[key] = n
	return n
}

var pciIDFiles = []string{"/usr/share/hwdata/pci.ids", "/usr/share/misc/pci.ids", "/usr/share/pci.ids"}

// pciName finds "Vendor Device" in pci.ids, shortening the vendor ("Advanced Micro Devices, Inc.
// [AMD/ATI]" -> "AMD").
func pciName(vendor, device string) string {
	for _, p := range pciIDFiles {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		inVendor := false
		vname := ""
		for sc.Scan() {
			line := sc.Text()
			if line == "" || line[0] == '#' {
				continue
			}
			if line[0] != '\t' {
				if inVendor {
					return ""
				}
				if strings.HasPrefix(line, vendor+"  ") {
					inVendor, vname = true, line[len(vendor)+2:]
				}
				continue
			}
			if inVendor && len(line) > 1 && line[1] != '\t' && strings.HasPrefix(line[1:], device+"  ") {
				return shortVendor(vname) + " " + line[1+len(device)+2:]
			}
		}
		return ""
	}
	return ""
}

func shortVendor(v string) string {
	switch {
	case strings.Contains(v, "AMD"), strings.Contains(v, "ATI"):
		return "AMD"
	case strings.Contains(v, "Intel"):
		return "Intel"
	case strings.Contains(v, "NVIDIA"):
		return "NVIDIA"
	}
	return v
}

// ---- DRM fdinfo ----

type drmClient struct {
	pid    int32
	id     string // drm-client-id
	driver string
	pdev   string
	busy   map[string]uint64 // drm-engine-<name>: <ns> ns
	cyc    map[string]uint64 // drm-cycles-<name>
	total  map[string]uint64 // drm-total-cycles-<name>
	mem    uint64            // video memory in bytes
}

// scanFdinfo reads every open DRM file's fdinfo. With pid set, only that process is read.
// Clients shared between several fds (or processes) are counted once.
func scanFdinfo(pid string) map[string]*drmClient {
	out := map[string]*drmClient{}
	procs := []string{pid}
	if pid == "" {
		procs, _ = filepath.Glob("/proc/[0-9]*")
		for i := range procs {
			procs[i] = filepath.Base(procs[i])
		}
	}
	for _, p := range procs {
		fds, err := os.ReadDir("/proc/" + p + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink("/proc/" + p + "/fd/" + fd.Name())
			if err != nil || !strings.HasPrefix(target, "/dev/dri/") {
				continue
			}
			b, err := os.ReadFile("/proc/" + p + "/fdinfo/" + fd.Name())
			if err != nil {
				continue
			}
			c := parseFdinfo(string(b))
			if c == nil {
				continue
			}
			n, _ := strconv.Atoi(p)
			c.pid = int32(n)
			id := c.id
			if id == "" {
				id = p + "-" + fd.Name()
			}
			key := c.pdev + "/" + id
			if _, dup := out[key]; !dup {
				out[key] = c
			}
		}
	}
	return out
}

// parseFdinfo reads the drm-* keys (see the kernel's drm-usage-stats documentation).
func parseFdinfo(s string) *drmClient {
	c := &drmClient{busy: map[string]uint64{}, cyc: map[string]uint64{}, total: map[string]uint64{}}
	found := false
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(k, "drm-") {
			continue
		}
		found = true
		v = strings.TrimSpace(v)
		switch {
		case k == "drm-driver":
			c.driver = v
		case k == "drm-pdev":
			c.pdev = v
		case k == "drm-client-id":
			c.id = v
		case strings.HasPrefix(k, "drm-engine-") && !strings.HasPrefix(k, "drm-engine-capacity-"):
			c.busy[strings.TrimPrefix(k, "drm-engine-")] = firstUint(v)
		case strings.HasPrefix(k, "drm-total-cycles-"):
			c.total[strings.TrimPrefix(k, "drm-total-cycles-")] = firstUint(v)
		case strings.HasPrefix(k, "drm-cycles-"):
			c.cyc[strings.TrimPrefix(k, "drm-cycles-")] = firstUint(v)
		case k == "drm-memory-vram", k == "drm-total-vram", k == "drm-total-local", k == "drm-resident-local":
			if n := memBytes(v); n > c.mem {
				c.mem = n
			}
		}
	}
	if !found {
		return nil
	}
	return c
}

func firstUint(v string) uint64 {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseUint(f[0], 10, 64)
	return n
}

// memBytes reads "123 KiB" / "4 MiB" / "512" values.
func memBytes(v string) uint64 {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0
	}
	if len(f) > 1 {
		switch f[1] {
		case "KiB":
			n <<= 10
		case "MiB":
			n <<= 20
		case "GiB":
			n <<= 30
		}
	}
	return n
}

// drmTotals sums the clients' engine counters per card (PCI slot).
func drmTotals(clients map[string]*drmClient) map[string]clientSample {
	out := map[string]clientSample{}
	for _, c := range clients {
		s, ok := out[c.pdev]
		if !ok {
			s = clientSample{busy: map[string]uint64{}, cyc: map[string]uint64{}, total: map[string]uint64{}}
		}
		for e, v := range c.busy {
			s.busy[e] += v
		}
		for e, v := range c.cyc {
			s.cyc[e] += v
		}
		for e, v := range c.total {
			if v > s.total[e] { // the GPU's total cycles: the same for every client
				s.total[e] = v
			}
		}
		out[c.pdev] = s
	}
	return out
}

// engineLoad is the busiest engine's % of the time between two samples.
func engineLoad(prev, cur clientSample) float64 {
	best := 0.0
	dt := float64(cur.at.Sub(prev.at).Nanoseconds())
	for e, v := range cur.busy {
		if p, ok := prev.busy[e]; ok && v >= p && dt > 0 {
			if pct := float64(v-p) / dt * 100; pct > best {
				best = pct
			}
		}
	}
	for e, v := range cur.cyc {
		p, ok := prev.cyc[e]
		tt, tp := cur.total[e], prev.total[e]
		if ok && v >= p && tt > tp {
			if pct := float64(v-p) / float64(tt-tp) * 100; pct > best {
				best = pct
			}
		}
	}
	if best > 100 {
		best = 100
	}
	return float64(int(best*10+0.5)) / 10
}

func (l *linuxGPU) procUsage(skipNVIDIA bool) map[int32]procGPU {
	first := scanFdinfo("")
	if len(first) == 0 {
		return nil
	}
	t0 := time.Now()
	time.Sleep(time.Second)
	second := map[string]*drmClient{}
	pids := map[int32]bool{}
	for _, c := range first {
		pids[c.pid] = true
	}
	for pid := range pids {
		for k, c := range scanFdinfo(strconv.Itoa(int(pid))) {
			second[k] = c
		}
	}
	dt := time.Since(t0)
	out := map[int32]procGPU{}
	for k, c := range second {
		if skipNVIDIA && c.driver == "nvidia" {
			continue
		}
		u := procGPU{Mem: c.mem}
		if p, ok := first[k]; ok {
			a := clientSample{at: t0, busy: p.busy, cyc: p.cyc, total: p.total}
			b := clientSample{at: t0.Add(dt), busy: c.busy, cyc: c.cyc, total: c.total}
			u.Pct = engineLoad(a, b)
		}
		out[c.pid] = addProc(out[c.pid], u)
	}
	return out
}
