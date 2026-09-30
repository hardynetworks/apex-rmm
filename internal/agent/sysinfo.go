package agent

import (
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

var skipFS = map[string]bool{"squashfs": true, "tmpfs": true, "devtmpfs": true, "overlay": true, "iso9660": true, "autofs": true, "nullfs": true, "devfs": true, "udf": true}

func collectDisks() []proto.Disk {
	parts, _ := disk.Partitions(false)
	seen := map[string]bool{}
	hasMacData := false
	for _, p := range parts {
		if p.Mountpoint == "/System/Volumes/Data" {
			hasMacData = true
		}
	}
	var out []proto.Disk
	for _, p := range parts {
		m := p.Mountpoint
		if skipFS[strings.ToLower(p.Fstype)] || seen[m] {
			continue
		}
		if runtime.GOOS == "linux" && (strings.HasPrefix(m, "/snap") || strings.HasPrefix(m, "/var/lib/docker") || strings.HasPrefix(m, "/run") || strings.HasPrefix(m, "/boot/efi")) {
			continue
		}
		if runtime.GOOS == "darwin" {
			if strings.HasPrefix(m, "/System/Volumes/") && m != "/System/Volumes/Data" {
				continue
			}
			if m == "/" && hasMacData {
				continue
			}
		}
		u, err := disk.Usage(m)
		if err != nil || u.Total == 0 {
			continue
		}
		seen[m] = true
		out = append(out, proto.Disk{Mount: m, FSType: p.Fstype, Total: u.Total, Used: u.Used, Percent: round1(u.UsedPercent)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

var (
	usersMu    sync.Mutex
	usersCache []string
	usersAt    time.Time
)

// loggedInUsers returns interactive users (cached for 2 minutes).
func loggedInUsers() []string {
	usersMu.Lock()
	defer usersMu.Unlock()
	if time.Since(usersAt) < 2*time.Minute && usersCache != nil {
		return usersCache
	}
	set := map[string]bool{}
	if runtime.GOOS == "windows" {
		for _, u := range windowsUsers() {
			set[u] = true
		}
	} else if us, err := host.Users(); err == nil {
		for _, u := range us {
			if u.User != "" {
				set[u.User] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	usersCache, usersAt = out, time.Now()
	return out
}

// CollectInventory gathers static device information.
func CollectInventory() proto.Inventory {
	inv := proto.Inventory{OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: proto.Version}
	inv.Hostname, _ = os.Hostname()
	if hi, err := host.Info(); err == nil {
		inv.Hostname = firstNonEmpty(hi.Hostname, inv.Hostname)
		inv.Platform = hi.Platform
		inv.OSVersion = hi.PlatformVersion
		inv.KernelVersion = hi.KernelVersion
		inv.BootTime = int64(hi.BootTime)
		if hi.VirtualizationRole == "guest" {
			inv.Virtual = hi.VirtualizationSystem
		}
	}
	if ci, err := cpu.Info(); err == nil && len(ci) > 0 {
		inv.CPUModel = strings.TrimSpace(ci[0].ModelName)
	}
	inv.CPUCores, _ = cpu.Counts(false)
	inv.CPUThreads, _ = cpu.Counts(true)
	if vm, err := mem.VirtualMemory(); err == nil {
		inv.RAMTotal = vm.Total
	}
	inv.Disks = collectDisks()
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			if i.HardwareAddr == "" && len(i.Addrs) == 0 {
				continue
			}
			isLoop := false
			for _, f := range i.Flags {
				if f == "loopback" {
					isLoop = true
				}
			}
			if isLoop || strings.HasPrefix(i.Name, "veth") || strings.HasPrefix(i.Name, "docker") || strings.HasPrefix(i.Name, "br-") {
				continue
			}
			n := proto.NIC{Name: i.Name, MAC: i.HardwareAddr}
			for _, a := range i.Addrs {
				n.Addrs = append(n.Addrs, a.Addr)
			}
			inv.NICs = append(inv.NICs, n)
		}
	}
	inv.LoggedInUsers = loggedInUsers()
	inv.Manufacturer, inv.Model, inv.Serial = hardwareInfo()
	if runtime.GOOS == "windows" && inv.Platform != "" {
		inv.Platform = strings.TrimSpace(inv.Platform)
	}
	return inv
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// metricsCollector keeps state for rate calculations.
type metricsCollector struct {
	lastRx, lastTx uint64
	lastAt         time.Time
}

func newMetricsCollector() *metricsCollector {
	_, _ = cpu.Percent(0, false) // prime
	return &metricsCollector{}
}

func (mc *metricsCollector) collect() proto.Metrics {
	var m proto.Metrics
	if p, err := cpu.Percent(0, false); err == nil && len(p) > 0 {
		m.CPU = round1(p[0])
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		m.Mem = round1(vm.UsedPercent)
		m.MemUsed = vm.Used
	}
	m.Disks = collectDisks()
	for _, d := range m.Disks {
		if d.Percent > m.Disk {
			m.Disk = d.Percent
		}
	}
	if io, err := net.IOCounters(false); err == nil && len(io) > 0 {
		now := time.Now()
		if !mc.lastAt.IsZero() {
			secs := now.Sub(mc.lastAt).Seconds()
			if secs > 0 && io[0].BytesRecv >= mc.lastRx && io[0].BytesSent >= mc.lastTx {
				m.NetRx = uint64(float64(io[0].BytesRecv-mc.lastRx) / secs)
				m.NetTx = uint64(float64(io[0].BytesSent-mc.lastTx) / secs)
			}
		}
		mc.lastRx, mc.lastTx, mc.lastAt = io[0].BytesRecv, io[0].BytesSent, now
	}
	if up, err := host.Uptime(); err == nil {
		m.Uptime = up
	}
	if l, err := load.Avg(); err == nil {
		m.Load1 = l.Load1
	}
	m.Users = loggedInUsers()
	return m
}
