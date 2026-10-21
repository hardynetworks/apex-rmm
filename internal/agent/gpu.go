package agent

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// GPU monitoring. NVIDIA cards are read with nvidia-smi (ships with the driver on Windows and
// Linux); everything else comes from the OS: sysfs and DRM fdinfo on Linux, performance counters
// and DXGI on Windows, ioreg on macOS (gpu_<os>.go).

// platformGPU is implemented per OS.
type platformGPU interface {
	// gpus returns the adapters the OS can report. skipNVIDIA is set when nvidia-smi already
	// covered the NVIDIA cards.
	gpus(skipNVIDIA bool) []proto.GPU
	// procUsage measures per-process GPU use (blocking for about a second). skipNVIDIA as above.
	procUsage(skipNVIDIA bool) map[int32]procGPU
	// names lists the adapters for the inventory.
	names() []string
}

type procGPU struct {
	Pct float64 // % of one GPU's time on its busiest engine
	Mem uint64  // bytes of video memory
}

type gpuMonitor struct {
	mu     sync.Mutex
	nvidia cmdProbe
	plat   platformGPU
}

var (
	gpuOnce sync.Once
	gpuMon  *gpuMonitor
)

func gpuMonitorInstance() *gpuMonitor {
	gpuOnce.Do(func() { gpuMon = &gpuMonitor{plat: newPlatformGPU()} })
	return gpuMon
}

// collect returns every GPU's current state, NVIDIA cards first.
func (g *gpuMonitor) collect() []proto.GPU {
	g.mu.Lock()
	defer g.mu.Unlock()
	nv := g.nvidiaGPUs()
	out := append(nv, g.plat.gpus(len(nv) > 0)...)
	return out
}

// names lists the graphics adapters for the inventory.
func (g *gpuMonitor) names() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	nv := g.nvidiaGPUs()
	for _, x := range nv {
		out = append(out, x.Name)
	}
	for _, n := range g.plat.names() {
		if len(nv) > 0 && isNVIDIAName(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// procUsage merges NVIDIA's per-process figures with the OS's.
func (g *gpuMonitor) procUsage() map[int32]procGPU {
	g.mu.Lock()
	defer g.mu.Unlock()
	nvOK := false
	out := map[int32]procGPU{}
	if bin := g.nvidia.find(nvidiaSMIPaths()...); bin != "" && nvidiaPmonUseful {
		if s, err := runCmd(10*time.Second, bin, "pmon", "-c", "1", "-s", "um"); err == nil {
			nvOK = true
			for pid, u := range parseNvidiaPmon(s) {
				out[pid] = addProc(out[pid], u)
			}
		}
	}
	for pid, u := range g.plat.procUsage(nvOK) {
		out[pid] = addProc(out[pid], u)
	}
	return out
}

func addProc(a, b procGPU) procGPU {
	if b.Pct > a.Pct {
		a.Pct = b.Pct
	}
	a.Mem += b.Mem
	return a
}

func isNVIDIAName(n string) bool {
	n = strings.ToLower(n)
	return strings.Contains(n, "nvidia") || strings.Contains(n, "geforce") || strings.Contains(n, "quadro") || strings.Contains(n, "tesla")
}

// ---- NVIDIA ----

const nvidiaQuery = "index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,fan.speed,clocks.gr,clocks.mem,power.draw"

func (g *gpuMonitor) nvidiaGPUs() []proto.GPU {
	bin := g.nvidia.find(nvidiaSMIPaths()...)
	if bin == "" {
		return nil
	}
	out, err := runCmd(8*time.Second, bin, "--query-gpu="+nvidiaQuery, "--format=csv,noheader,nounits")
	if err != nil {
		g.nvidia.failed()
		return nil
	}
	return parseNvidiaQuery(out)
}

// parseNvidiaQuery reads `nvidia-smi --query-gpu=<nvidiaQuery> --format=csv,noheader,nounits`.
// The name can contain commas, so the eight numbers are taken from the right.
func parseNvidiaQuery(out string) []proto.GPU {
	var gpus []proto.GPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 10 {
			continue
		}
		n := len(f)
		num := func(i int) float64 { return nvNum(f[n-8+i]) }
		g := proto.GPU{
			Name:   strings.TrimSpace(strings.Join(f[1:n-8], ",")),
			Vendor: "nvidia", Source: "nvidia-smi",
			Load: num(0), TempC: num(3), FanPct: num(4), Watts: num(7),
		}
		if v := num(1); v >= 0 {
			g.MemUsed = uint64(v) << 20 // MiB
		}
		if v := num(2); v >= 0 {
			g.MemTotal = uint64(v) << 20
		}
		if v := num(5); v > 0 {
			g.ClockMHz = v
		}
		if v := num(6); v > 0 {
			g.MemClock = v
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// nvNum parses one nvidia-smi value; "[N/A]", "[Not Supported]" and the like are -1.
func nvNum(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return v
}

// parseNvidiaPmon reads `nvidia-smi pmon -c 1 -s um`. Columns differ between driver versions,
// so they are found by the header line ("# gpu pid type sm mem enc dec ... fb ... command").
func parseNvidiaPmon(out string) map[int32]procGPU {
	res := map[int32]procGPU{}
	col := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		t := strings.Fields(line)
		if len(t) == 0 {
			continue
		}
		if t[0] == "#" || strings.HasPrefix(t[0], "#") {
			if len(col) == 0 {
				hdr := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "#"))
				for i, h := range hdr {
					col[strings.ToLower(h)] = i
				}
			}
			continue
		}
		pi, ok := col["pid"]
		if !ok || pi >= len(t) {
			continue
		}
		pid, err := strconv.Atoi(t[pi])
		if err != nil || pid <= 0 {
			continue
		}
		get := func(name string) float64 {
			i, ok := col[name]
			if !ok || i >= len(t) {
				return 0
			}
			v, err := strconv.ParseFloat(t[i], 64)
			if err != nil {
				return 0
			}
			return v
		}
		u := procGPU{Pct: get("sm")}
		if fb := get("fb"); fb > 0 {
			u.Mem = uint64(fb) << 20
		}
		res[int32(pid)] = addProc(res[int32(pid)], u)
	}
	return res
}

// nvidiaPmonUseful: on Windows the performance counters already cover NVIDIA cards per process.
var nvidiaPmonUseful = true
