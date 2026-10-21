//go:build linux

package agent

import "testing"

func TestLinuxSysfsGPUs(t *testing.T) {
	root := t.TempDir()
	old := sysRoot
	sysRoot = root
	defer func() { sysRoot = old }()
	writeSys(t, root, map[string]string{
		"class/drm/card0/device/vendor":                      "0x1002",
		"class/drm/card0/device/device":                      "0x73bf",
		"class/drm/card0/device/gpu_busy_percent":            "42",
		"class/drm/card0/device/mem_info_vram_total":         "17163091968",
		"class/drm/card0/device/mem_info_vram_used":          "1073741824",
		"class/drm/card0/device/hwmon/hwmon2/temp1_input":    "55000",
		"class/drm/card0/device/hwmon/hwmon2/pwm1":           "128",
		"class/drm/card0/device/hwmon/hwmon2/power1_average": "48000000",
		"class/drm/card0/device/hwmon/hwmon2/freq1_input":    "2100000000",
		"class/drm/card0-DP-1/status":                        "connected",
		"class/drm/card1/device/vendor":                      "0x15ad", // VMware SVGA: not a GPU
		"class/drm/card1/device/device":                      "0x0405",
	})
	l := newPlatformGPU().(*linuxGPU)
	g := l.gpus(false)
	if len(g) != 1 {
		t.Fatalf("%+v", g)
	}
	a := g[0]
	if a.Vendor != "amd" || a.Load != 42 || a.MemTotal != 17163091968 || a.MemUsed != 1<<30 || a.TempC != 55 || a.FanPct != 50 || a.Watts != 48 || a.ClockMHz != 2100 {
		t.Fatalf("%+v", a)
	}
	if a.Name == "" {
		t.Fatal("no name")
	}
}
