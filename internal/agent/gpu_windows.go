//go:build windows

package agent

import (
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/hardynetworks/apex-rmm/internal/proto"
	"golang.org/x/sys/windows"
)

// Windows GPUs: the adapters come from DXGI (name, vendor, dedicated memory, LUID) and their use
// from the "GPU Engine", "GPU Adapter Memory" and "GPU Process Memory" performance counters,
// the same figures Task Manager shows. Temperatures need the vendor's tool: nvidia-smi for
// NVIDIA, ADL (part of the Radeon driver) for AMD power, temperature, fan and clocks.

var (
	gpuDXGI              = windows.NewLazySystemDLL("dxgi.dll")
	gpuCreateDXGIFactory = gpuDXGI.NewProc("CreateDXGIFactory1")
	gpuIIDFactory1       = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
)

const (
	gpuVtRelease       = 2
	gpuVtEnumAdapters1 = 12 // IDXGIFactory1::EnumAdapters1
	gpuVtGetDesc1      = 10 // IDXGIAdapter1::GetDesc1
	gpuDXGINotFound    = 0x887A0002
	gpuAdapterSoftware = 2 // DXGI_ADAPTER_FLAG_SOFTWARE
)

type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	LuidLow               uint32
	LuidHigh              int32
	Flags                 uint32
}

type winAdapter struct {
	name      string
	vendor    uint32
	dedicated uint64
	shared    uint64
	luid      string // "0x00000000_0x0000d1a6", as in the counter instance names
}

type winGPU struct {
	mu       sync.Mutex
	adapters []winAdapter
	listed   time.Time
	engine   pdhCounter // \GPU Engine(*)\Utilization Percentage
	dedic    pdhCounter // \GPU Adapter Memory(*)\Dedicated Usage
	share    pdhCounter // \GPU Adapter Memory(*)\Shared Usage
}

func newPlatformGPU() platformGPU {
	nvidiaPmonUseful = false // the counters below already cover NVIDIA processes
	g := &winGPU{
		engine: pdhCounter{path: `\GPU Engine(*)\Utilization Percentage`},
		dedic:  pdhCounter{path: `\GPU Adapter Memory(*)\Dedicated Usage`},
		share:  pdhCounter{path: `\GPU Adapter Memory(*)\Shared Usage`},
	}
	g.mu.Lock()
	g.engine.read() // rate counter: the first sample only primes it
	g.mu.Unlock()
	return g
}

func gpuPtr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

func gpuCOM(obj uintptr, slot int, args ...uintptr) uint32 {
	vtbl := *(*uintptr)(gpuPtr(obj))
	fn := *(*uintptr)(gpuPtr(vtbl + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return uint32(r)
}

// listAdapters enumerates the hardware adapters through DXGI (cached for 10 minutes).
func (g *winGPU) listAdapters() []winAdapter {
	if time.Since(g.listed) < 10*time.Minute {
		return g.adapters
	}
	g.listed = time.Now()
	g.adapters = nil
	if gpuCreateDXGIFactory.Find() != nil {
		return nil
	}
	var factory uintptr
	if r, _, _ := gpuCreateDXGIFactory.Call(uintptr(unsafe.Pointer(&gpuIIDFactory1)), uintptr(unsafe.Pointer(&factory))); int32(r) < 0 || factory == 0 {
		return nil
	}
	defer gpuCOM(factory, gpuVtRelease)
	seen := map[string]bool{}
	for i := uint32(0); i < 16; i++ {
		var a uintptr
		if hr := gpuCOM(factory, gpuVtEnumAdapters1, uintptr(i), uintptr(unsafe.Pointer(&a))); hr == gpuDXGINotFound || int32(hr) < 0 {
			break
		}
		var d dxgiAdapterDesc1
		hr := gpuCOM(a, gpuVtGetDesc1, uintptr(unsafe.Pointer(&d)))
		gpuCOM(a, gpuVtRelease)
		if int32(hr) < 0 || d.Flags&gpuAdapterSoftware != 0 || d.VendorID == 0x1414 { // Microsoft Basic Render Driver
			continue
		}
		luid := strings.ToLower("0x" + hex8(uint32(d.LuidHigh)) + "_0x" + hex8(d.LuidLow))
		if seen[luid] {
			continue
		}
		seen[luid] = true
		g.adapters = append(g.adapters, winAdapter{
			name: strings.TrimSpace(windows.UTF16ToString(d.Description[:])), vendor: d.VendorID,
			dedicated: uint64(d.DedicatedVideoMemory), shared: uint64(d.SharedSystemMemory), luid: luid,
		})
	}
	return g.adapters
}

func hex8(v uint32) string {
	s := strconv.FormatUint(uint64(v), 16)
	return strings.Repeat("0", 8-len(s)) + s
}

func winVendor(v uint32) string {
	switch v {
	case 0x10de:
		return "nvidia"
	case 0x1002, 0x1022:
		return "amd"
	case 0x8086:
		return "intel"
	case 0x15ad, 0x1234, 0x1af4, 0x1b36, 0x80ee, 0x1a03, 0x102b:
		return "virtual"
	}
	return "other"
}

func (g *winGPU) names() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, a := range g.listAdapters() {
		if winVendor(a.vendor) != "virtual" {
			out = append(out, a.name)
		}
	}
	return out
}

func (g *winGPU) gpus(skipNVIDIA bool) []proto.GPU {
	g.mu.Lock()
	defer g.mu.Unlock()
	adapters := g.listAdapters()
	if len(adapters) == 0 {
		return nil
	}
	load, _ := engineUse(g.engine.read())
	ded, sh := map[string]float64{}, map[string]float64{}
	for inst, v := range g.dedic.read() {
		if m := reAdMem.FindStringSubmatch(inst); m != nil {
			ded[strings.ToLower(m[1])] += v
		}
	}
	for inst, v := range g.share.read() {
		if m := reAdMem.FindStringSubmatch(inst); m != nil {
			sh[strings.ToLower(m[1])] += v
		}
	}
	var out []proto.GPU
	for _, a := range adapters {
		vend := winVendor(a.vendor)
		if vend == "virtual" || (vend == "nvidia" && skipNVIDIA) {
			continue
		}
		x := proto.GPU{Name: a.name, Vendor: vend, Load: -1, TempC: -1, FanPct: -1, Watts: -1, Source: "pdh"}
		if v, ok := load[a.luid]; ok {
			x.Load = clampPct(v)
		} else if g.engine.query != 0 {
			x.Load = 0 // counters work; this adapter just had no engine activity
		}
		// Integrated GPUs have little or no dedicated memory and use shared system memory.
		if a.dedicated >= 256<<20 {
			x.MemTotal = a.dedicated
			x.MemUsed = uint64(ded[a.luid])
		} else {
			x.Shared = true
			x.MemTotal = a.shared
			x.MemUsed = uint64(sh[a.luid] + ded[a.luid])
		}
		out = append(out, x)
	}
	addADL(out)
	return out
}

// addADL fills AMD GPUs' power, temperature, fan and clocks from ADL.
func addADL(gpus []proto.GPU) {
	var names []string
	var at []int
	for i, x := range gpus {
		if x.Vendor == "amd" {
			names, at = append(names, x.Name), append(at, i)
		}
	}
	if len(names) == 0 {
		return
	}
	rs := amdADL.readings()
	for k, j := range matchADL(names, rs) {
		if j < 0 {
			continue
		}
		r, x := rs[j], &gpus[at[k]]
		if r.Watts >= 0 {
			x.Watts = round1(r.Watts)
		}
		if r.TempC >= 0 {
			x.TempC = r.TempC
		}
		if r.FanPct >= 0 {
			x.FanPct = r.FanPct
		}
		if r.ClockMHz > 0 {
			x.ClockMHz = r.ClockMHz
		}
		if r.MemMHz > 0 {
			x.MemClock = r.MemMHz
		}
		if x.Load < 0 && r.Load >= 0 {
			x.Load = r.Load
		}
		x.Source = "pdh+adl"
	}
}

// amdPowerParts are the AMD GPUs' power draw for the device's energy reading. An integrated
// Radeon's figure is part of the CPU package's, so it is named to be left out of the sum.
func amdPowerParts() []proto.PowerPart {
	g, ok := gpuMonitorInstance().plat.(*winGPU)
	if !ok {
		return nil
	}
	g.mu.Lock()
	var names []string
	var integrated []bool
	for _, a := range g.listAdapters() {
		if winVendor(a.vendor) == "amd" {
			names = append(names, a.name)
			integrated = append(integrated, a.dedicated < 256<<20)
		}
	}
	g.mu.Unlock()
	if len(names) == 0 {
		return nil
	}
	rs := amdADL.readings()
	var parts []proto.PowerPart
	for k, j := range matchADL(names, rs) {
		if j < 0 || rs[j].Watts < 0 {
			continue
		}
		name := names[k]
		if integrated[k] {
			name = "Integrated GPU (" + name + ")"
		}
		parts = append(parts, proto.PowerPart{Name: name, Watts: rs[j].Watts, Source: "adl"})
	}
	return parts
}

func (g *winGPU) procUsage(skipNVIDIA bool) map[int32]procGPU {
	eng := pdhCounter{path: `\GPU Engine(*)\Utilization Percentage`}
	mem := pdhCounter{path: `\GPU Process Memory(*)\Dedicated Usage`}
	eng.read()
	time.Sleep(time.Second)
	_, perProc := engineUse(eng.read())
	memVals := mem.read()
	if mem.query == 0 {
		memVals = nil
	} else if memVals == nil {
		memVals = mem.read()
	}
	closePDH(&eng)
	closePDH(&mem)
	out := map[int32]procGPU{}
	for pid, v := range perProc {
		out[pid] = procGPU{Pct: clampPct(v)}
	}
	for inst, v := range memVals {
		m := reProcMem.FindStringSubmatch(inst)
		if m == nil || v <= 0 {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		u := out[int32(pid)]
		u.Mem += uint64(v)
		out[int32(pid)] = u
	}
	return out
}

func closePDH(c *pdhCounter) {
	if c.query != 0 {
		pPdhCloseQuery.Call(c.query)
		c.query = 0
	}
}
