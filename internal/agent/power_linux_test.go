//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSys(t *testing.T, root string, files map[string]string) {
	for p, v := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinuxSysfs(t *testing.T) {
	root := t.TempDir()
	old := sysRoot
	sysRoot = root
	defer func() { sysRoot = old }()
	writeSys(t, root, map[string]string{
		"class/powercap/intel-rapl:0/name":                "package-0",
		"class/powercap/intel-rapl:0/energy_uj":           "1000000",
		"class/powercap/intel-rapl:0/max_energy_range_uj": "262143328850",
		"class/powercap/intel-rapl:0:0/name":              "core",
		"class/powercap/intel-rapl:0:0/energy_uj":         "500000",
		"class/powercap/intel-rapl:0:2/name":              "dram",
		"class/powercap/intel-rapl:0:2/energy_uj":         "100000",
		"class/hwmon/hwmon3/name":                         "amdgpu",
		"class/hwmon/hwmon3/power1_average":               "35000000",
		"class/power_supply/AC/type":                      "Mains",
		"class/power_supply/AC/online":                    "1",
		"class/power_supply/BAT0/type":                    "Battery",
		"class/power_supply/BAT0/status":                  "Charging",
		"class/power_supply/BAT0/power_now":               "9000000",
	})
	p := newPlatformPower().(*linuxPower)
	for _, c := range p.rapl { // pretend the first read was 2 s ago
		c.at = c.at.Add(-2 * time.Second)
	}
	writeSys(t, root, map[string]string{
		"class/powercap/intel-rapl:0/energy_uj":   "41000000", // 40 J in 2 s = 20 W
		"class/powercap/intel-rapl:0:2/energy_uj": "6100000",  // 6 J = 3 W
	})
	parts := p.parts()
	if len(parts) != 3 || parts[0].Name != "CPU package" || !near(parts[0].Watts, 20) || parts[1].Name != "RAM" || !near(parts[1].Watts, 3) ||
		parts[2].Name != "AMD GPU" || !near(parts[2].Watts, 35) {
		t.Fatalf("%+v", parts)
	}
	if _, _, ok := p.whole(); ok {
		t.Fatal("charging laptop on AC reported whole-system power")
	}
	if !hasBattery() || p.onBattery() {
		t.Fatal("battery state")
	}
	// unplugged and discharging: the battery rate is the whole system
	writeSys(t, root, map[string]string{"class/power_supply/AC/online": "0", "class/power_supply/BAT0/status": "Discharging"})
	if w, src, ok := p.whole(); !ok || src != "battery" || !near(w, 9) {
		t.Fatal(w, src, ok)
	}
	// an ACPI power meter wins over the battery
	writeSys(t, root, map[string]string{"class/hwmon/hwmon1/name": "power_meter", "class/hwmon/hwmon1/power1_average": "212000000"})
	if w, src, ok := p.whole(); !ok || src != "meter" || !near(w, 212) {
		t.Fatal(w, src, ok)
	}
}
