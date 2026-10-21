//go:build darwin

package agent

import (
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// macOS GPUs: names and video memory from system_profiler (once), load and memory in use from
// the IOAccelerator entries' PerformanceStatistics in ioreg. Temperature and per-process use
// need private APIs or powermetrics and are not reported.

type macGPU struct {
	once  sync.Once
	cards []macCard
}

func newPlatformGPU() platformGPU { return &macGPU{} }

func (m *macGPU) load() {
	m.once.Do(func() {
		out, err := runCmd(30*time.Second, "system_profiler", "-json", "SPDisplaysDataType")
		if err == nil {
			m.cards = parseSPDisplays(out)
		}
	})
}

func (m *macGPU) names() []string {
	m.load()
	var out []string
	for _, c := range m.cards {
		out = append(out, c.name)
	}
	return out
}

func (m *macGPU) gpus(skipNVIDIA bool) []proto.GPU {
	m.load()
	out, err := runCmd(10*time.Second, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	var acc []ioregAccel
	if err == nil {
		acc = parseIoregAccel(out)
	}
	var res []proto.GPU
	for i, c := range m.cards {
		g := proto.GPU{Name: c.name, Vendor: c.vendor, Load: -1, TempC: -1, FanPct: -1, Watts: -1, Source: "ioreg", Shared: c.shared}
		if c.vram > 0 {
			g.MemTotal = c.vram
		}
		if len(acc) == len(m.cards) { // matched by order only when the counts agree
			g.Load = acc[i].load
			g.MemUsed = acc[i].mem
		}
		res = append(res, g)
	}
	if len(m.cards) == 0 {
		for _, a := range acc {
			res = append(res, proto.GPU{Name: "GPU", Vendor: "other", Load: a.load, MemUsed: a.mem, TempC: -1, FanPct: -1, Watts: -1, Source: "ioreg"})
		}
	}
	return res
}

func (m *macGPU) procUsage(bool) map[int32]procGPU { return nil }
