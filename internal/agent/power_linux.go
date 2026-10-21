//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// Linux power sources:
//   - IPMI via ipmitool ("dcmi power reading") when the server has a BMC (/dev/ipmi0)
//   - ACPI power meter (hwmon "power_meter", found on many servers)
//   - battery discharge rate (/sys/class/power_supply) on laptops running on battery
//   - RAPL energy counters (/sys/class/powercap, Intel and AMD Zen): CPU package and RAM
//   - hwmon power sensors of AMD GPUs (amdgpu)
type linuxPower struct {
	mu       sync.Mutex
	rapl     map[string]*energyCounter
	ipmi     cmdProbe
	ipmiNext time.Time
	ipmiLast float64
	ipmiAt   time.Time
}

func newPlatformPower() platformPower {
	p := &linuxPower{rapl: map[string]*energyCounter{}}
	p.parts() // prime the energy counters so the first sample has a rate
	return p
}

var sysRoot = "/sys" // tests point this at a fake tree

func readNum(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return f, err == nil
}

func readStr(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func (p *linuxPower) whole() (float64, string, bool) {
	if w, ok := p.readIPMI(); ok {
		return w, "ipmi", true
	}
	// ACPI power meter: power1_average in µW
	hw, _ := filepath.Glob(filepath.Join(sysRoot, "class/hwmon/hwmon*"))
	for _, h := range hw {
		if readStr(filepath.Join(h, "name")) != "power_meter" {
			continue
		}
		for _, f := range []string{"power1_average", "power1_input"} {
			if v, ok := readNum(filepath.Join(h, f)); ok && v > 0 {
				return v / 1e6, "meter", true
			}
		}
	}
	if w, ok := batteryDischarge(); ok {
		return w, "battery", true
	}
	return 0, "", false
}

func (p *linuxPower) onBattery() bool {
	_, ok := batteryDischarge()
	return ok
}

// batteryDischarge is the draw of a laptop running on battery (and not on AC).
func batteryDischarge() (float64, bool) {
	supplies, _ := filepath.Glob(filepath.Join(sysRoot, "class/power_supply/*"))
	total, found := 0.0, false
	for _, s := range supplies {
		switch readStr(filepath.Join(s, "type")) {
		case "Mains", "USB":
			if readStr(filepath.Join(s, "online")) == "1" {
				return 0, false
			}
			continue
		case "Battery":
		default:
			continue
		}
		if readStr(filepath.Join(s, "scope")) == "Device" { // a mouse or keyboard battery
			continue
		}
		if readStr(filepath.Join(s, "status")) != "Discharging" {
			continue
		}
		if v, ok := readNum(filepath.Join(s, "power_now")); ok && v > 0 {
			total, found = total+v/1e6, true
			continue
		}
		c, ok1 := readNum(filepath.Join(s, "current_now"))
		v, ok2 := readNum(filepath.Join(s, "voltage_now"))
		if ok1 && ok2 && c != 0 {
			if c < 0 {
				c = -c
			}
			total, found = total+c/1e6*v/1e6, true
		}
	}
	return total, found
}

func hasBattery() bool {
	supplies, _ := filepath.Glob(filepath.Join(sysRoot, "class/power_supply/*"))
	for _, s := range supplies {
		if readStr(filepath.Join(s, "type")) == "Battery" && readStr(filepath.Join(s, "scope")) != "Device" {
			return true
		}
	}
	return false
}

var ipmiRe = regexp.MustCompile(`(?i)instantaneous power reading:\s*([\d.]+)\s*watts`)

// readIPMI asks the BMC through ipmitool (at most once a minute; it can take a second).
func (p *linuxPower) readIPMI() (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.ipmiAt) < 55*time.Second && p.ipmiLast > 0 {
		return p.ipmiLast, true
	}
	if time.Now().Before(p.ipmiNext) {
		return 0, false
	}
	dev := false
	for _, d := range []string{"/dev/ipmi0", "/dev/ipmi/0", "/dev/ipmidev/0"} {
		if _, err := os.Stat(d); err == nil {
			dev = true
		}
	}
	bin := ""
	if dev {
		bin = p.ipmi.find("ipmitool")
	}
	if bin == "" {
		p.ipmiNext = time.Now().Add(time.Hour)
		return 0, false
	}
	out, err := runCmd(8*time.Second, bin, "dcmi", "power", "reading")
	m := ipmiRe.FindStringSubmatch(out)
	if err != nil || m == nil {
		p.ipmiNext = time.Now().Add(time.Hour) // no DCMI power reading on this BMC
		return 0, false
	}
	w, _ := strconv.ParseFloat(m[1], 64)
	if w <= 0 {
		p.ipmiNext = time.Now().Add(10 * time.Minute)
		return 0, false
	}
	p.ipmiLast, p.ipmiAt = w, time.Now()
	return w, true
}

func (p *linuxPower) parts() []proto.PowerPart {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var parts []proto.PowerPart
	zones, _ := filepath.Glob(filepath.Join(sysRoot, "class/powercap/intel-rapl:*"))
	sort.Strings(zones)
	var pkgs, psys []proto.PowerPart
	for _, z := range zones {
		base := filepath.Base(z)
		if strings.Count(base, ":") > 2 {
			continue
		}
		name := readStr(filepath.Join(z, "name"))
		sub := strings.Count(base, ":") == 2
		if sub && name != "dram" { // core/uncore are inside the package figure
			continue
		}
		v, ok := readNum(filepath.Join(z, "energy_uj"))
		if !ok { // unreadable (needs root) or missing
			continue
		}
		c := p.rapl[base]
		if c == nil {
			c = &energyCounter{}
			if m, ok := readNum(filepath.Join(z, "max_energy_range_uj")); ok {
				c.max = uint64(m)
			}
			p.rapl[base] = c
		}
		w, ok := c.watts(uint64(v), now)
		if !ok {
			continue
		}
		label := name
		switch {
		case name == "dram":
			label = "RAM"
		case strings.HasPrefix(name, "package-"):
			label = "CPU package " + strings.TrimPrefix(name, "package-")
		case name == "psys":
			psys = append(psys, proto.PowerPart{Name: "Platform (SoC)", Watts: w, Source: "rapl"})
			continue
		}
		part := proto.PowerPart{Name: label, Watts: w, Source: "rapl"}
		if strings.HasPrefix(name, "package-") {
			pkgs = append(pkgs, part)
		} else {
			parts = append(parts, part)
		}
	}
	if len(pkgs) == 1 {
		pkgs[0].Name = "CPU package"
	}
	if len(pkgs) == 0 && len(psys) > 0 {
		pkgs = psys
	}
	parts = append(pkgs, parts...)

	// amdgpu (and other hwmon drivers that report power in µW)
	hw, _ := filepath.Glob(filepath.Join(sysRoot, "class/hwmon/hwmon*"))
	for _, h := range hw {
		name := readStr(filepath.Join(h, "name"))
		if name != "amdgpu" {
			continue
		}
		for _, f := range []string{"power1_average", "power1_input"} {
			if v, ok := readNum(filepath.Join(h, f)); ok && v > 0 {
				parts = append(parts, proto.PowerPart{Name: "AMD GPU", Watts: v / 1e6, Source: "hwmon"})
				break
			}
		}
	}
	return parts
}

func nvidiaSMIPaths() []string { return []string{"nvidia-smi"} }
