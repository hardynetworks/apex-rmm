//go:build windows

package agent

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/hardynetworks/apex-rmm/internal/proto"
	"golang.org/x/sys/windows"
)

// Windows power sources, without installing any driver:
//   - "Power Meter" performance counters: an ACPI power meter (whole system; some servers)
//   - IPMI through the in-box Microsoft IPMI driver (WMI Microsoft_IPMI, DCMI "Get Power
//     Reading"), on servers with a management board; asked through PowerShell once a minute
//   - "Energy Meter" performance counters: the CPU's RAPL counters (package, cores, RAM, iGPU)
//     exposed by Windows on supported Intel systems
var (
	pdhDLL                    = windows.NewLazySystemDLL("pdh.dll")
	pPdhOpenQuery             = pdhDLL.NewProc("PdhOpenQueryW")
	pPdhAddEnglishCounter     = pdhDLL.NewProc("PdhAddEnglishCounterW")
	pPdhCollectQueryData      = pdhDLL.NewProc("PdhCollectQueryData")
	pPdhGetFormattedCounterAr = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	pPdhCloseQuery            = pdhDLL.NewProc("PdhCloseQuery")

	kernel32DLL          = windows.NewLazySystemDLL("kernel32.dll")
	pGetSystemPowerState = kernel32DLL.NewProc("GetSystemPowerStatus")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtNoCap100 = 0x00008000
	pdhMoreData    = 0x800007D2
)

type pdhItem struct {
	name    *uint16
	cstatus uint32
	_       uint32
	value   float64
}

// pdhCounter is a wildcard counter ("\Energy Meter(*)\Power") kept open between samples.
type pdhCounter struct {
	path    string
	query   uintptr
	counter uintptr
	next    time.Time // retry time after a failure
}

func (c *pdhCounter) read() map[string]float64 {
	if c.query == 0 {
		if time.Now().Before(c.next) || pdhDLL.Load() != nil {
			return nil
		}
		var q uintptr
		if r, _, _ := pPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
			c.next = time.Now().Add(time.Hour)
			return nil
		}
		path, _ := windows.UTF16PtrFromString(c.path)
		var ctr uintptr
		if r, _, _ := pPdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&ctr))); r != 0 {
			pPdhCloseQuery.Call(q)
			c.next = time.Now().Add(time.Hour) // this counter set doesn't exist here
			return nil
		}
		c.query, c.counter = q, ctr
		pPdhCollectQueryData.Call(q) // rate counters need two samples
		return nil
	}
	if r, _, _ := pPdhCollectQueryData.Call(c.query); r != 0 {
		return nil
	}
	var size, count uint32
	r, _, _ := pPdhGetFormattedCounterAr.Call(c.counter, pdhFmtDouble|pdhFmtNoCap100, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(r) != pdhMoreData || size == 0 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ = pPdhGetFormattedCounterAr.Call(c.counter, pdhFmtDouble|pdhFmtNoCap100, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil
	}
	out := map[string]float64{}
	items := unsafe.Slice((*pdhItem)(unsafe.Pointer(&buf[0])), count)
	for _, it := range items {
		if it.cstatus != 0 && it.cstatus != 1 { // PDH_CSTATUS_VALID_DATA / NEW_DATA
			continue
		}
		out[windows.UTF16PtrToString(it.name)] = it.value
	}
	return out
}

type winPower struct {
	mu       sync.Mutex
	energy   pdhCounter
	meter    pdhCounter
	ipmiNext time.Time
	ipmiLast float64
	ipmiAt   time.Time
}

func newPlatformPower() platformPower {
	p := &winPower{energy: pdhCounter{path: `\Energy Meter(*)\Power`}, meter: pdhCounter{path: `\Power Meter(*)\Power`}}
	p.parts()
	p.mu.Lock()
	p.meter.read()
	p.mu.Unlock()
	return p
}

// Both counter sets report milliwatts.
func mwToW(v float64) (float64, bool) {
	w := v / 1000
	return w, w > 0 && w < 20000
}

func (p *winPower) whole() (float64, string, bool) {
	p.mu.Lock()
	vals := p.meter.read()
	p.mu.Unlock()
	if v, ok := vals["_Total"]; ok {
		if w, ok := mwToW(v); ok {
			return w, "meter", true
		}
	}
	sum, found := 0.0, false
	for k, v := range vals {
		if k == "_Total" {
			continue
		}
		if w, ok := mwToW(v); ok {
			sum, found = sum+w, true
		}
	}
	if found {
		return sum, "meter", true
	}
	if w, ok := p.readIPMI(); ok {
		return w, "ipmi", true
	}
	return 0, "", false
}

var raplInstance = regexp.MustCompile(`(?i)^RAPL_Package(\d+)_(PKG|PP0|PP1|DRAM|PSYS)$`)

func (p *winPower) parts() []proto.PowerPart {
	p.mu.Lock()
	vals := p.energy.read()
	p.mu.Unlock()
	var pkgs, rest []proto.PowerPart
	multi := false
	for k := range vals {
		if m := raplInstance.FindStringSubmatch(k); m != nil && m[1] != "0" {
			multi = true
		}
	}
	for k, v := range vals {
		m := raplInstance.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		w, ok := mwToW(v)
		if !ok {
			continue
		}
		suffix := ""
		if multi {
			suffix = " " + m[1]
		}
		switch strings.ToUpper(m[2]) {
		case "PKG":
			pkgs = append(pkgs, proto.PowerPart{Name: "CPU package" + suffix, Watts: w, Source: "rapl"})
		case "DRAM":
			rest = append(rest, proto.PowerPart{Name: "RAM" + suffix, Watts: w, Source: "rapl"})
		case "PP0":
			rest = append(rest, proto.PowerPart{Name: "CPU cores", Watts: w, Source: "rapl"})
		case "PP1":
			rest = append(rest, proto.PowerPart{Name: "Integrated GPU", Watts: w, Source: "rapl"})
		}
	}
	return append(pkgs, rest...)
}

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

func powerStatus() (systemPowerStatus, bool) {
	var s systemPowerStatus
	r, _, _ := pGetSystemPowerState.Call(uintptr(unsafe.Pointer(&s)))
	return s, r != 0
}

func (p *winPower) onBattery() bool {
	s, ok := powerStatus()
	return ok && s.ACLineStatus == 0 && s.BatteryFlag != 128 && s.BatteryFlag != 255
}

func hasBattery() bool {
	s, ok := powerStatus()
	return ok && s.BatteryFlag != 128 && s.BatteryFlag != 255
}

// DCMI Get Power Reading (NetFn 0x2C, command 0x02) through the Microsoft IPMI WMI provider.
const ipmiScript = `$ErrorActionPreference='Stop'
$i = Get-CimInstance -Namespace root/wmi -ClassName Microsoft_IPMI | Select-Object -First 1
$r = Invoke-CimMethod -InputObject $i -MethodName RequestResponse -Arguments @{Command=[byte]2;Lun=[byte]0;NetworkFunction=[byte]0x2C;RequestData=[byte[]](0xDC,1,0,0);RequestDataSize=[uint32]4;ResponderAddress=[byte]0x20}
[Console]::Out.Write("$($r.CompletionCode)|$($r.ResponseData -join ',')")`

func (p *winPower) readIPMI() (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.ipmiAt) < 55*time.Second && p.ipmiLast > 0 {
		return p.ipmiLast, true
	}
	if time.Now().Before(p.ipmiNext) {
		return 0, false
	}
	out, err := runCmd(15*time.Second, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ipmiScript)
	w, ok := parseDCMI(out)
	if err != nil || !ok {
		p.ipmiNext = time.Now().Add(6 * time.Hour) // no BMC (most PCs): don't keep asking
		return 0, false
	}
	p.ipmiLast, p.ipmiAt = w, time.Now()
	return w, true
}

func nvidiaSMIPaths() []string {
	pf := `C:\Program Files`
	if v := windowsEnv("ProgramFiles"); v != "" {
		pf = v
	}
	sys32, _ := windows.GetSystemDirectory()
	return []string{"nvidia-smi.exe", sys32 + `\nvidia-smi.exe`, fmt.Sprintf(`%s\NVIDIA Corporation\NVSMI\nvidia-smi.exe`, pf)}
}

func windowsEnv(k string) string {
	v, _ := windows.Getenv(k)
	return v
}
