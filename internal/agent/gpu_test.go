package agent

import (
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

func TestParseNvidiaQuery(t *testing.T) {
	out := "0, NVIDIA GeForce RTX 3080, 37, 2048, 10240, 61, 45, 1710, 9501, 220.35\n" +
		"1, Tesla T4, [N/A], 0, 15360, 40, [N/A], 300, 5000, [Not Supported]\n"
	g := parseNvidiaQuery(out)
	if len(g) != 2 {
		t.Fatalf("%+v", g)
	}
	a := g[0]
	if a.Name != "NVIDIA GeForce RTX 3080" || a.Vendor != "nvidia" || a.Load != 37 || a.MemUsed != 2048<<20 || a.MemTotal != 10240<<20 ||
		a.TempC != 61 || a.FanPct != 45 || a.ClockMHz != 1710 || a.MemClock != 9501 || !near(a.Watts, 220.35) {
		t.Fatalf("%+v", a)
	}
	b := g[1]
	if b.Name != "Tesla T4" || b.Load != -1 || b.FanPct != -1 || b.Watts != -1 || b.MemTotal != 15360<<20 {
		t.Fatalf("%+v", b)
	}
	// a name with a comma
	if g := parseNvidiaQuery("0, Quadro RTX 4000, Max-Q, 5, 100, 8192, 50, 30, 300, 405, 15.2"); len(g) != 1 || g[0].Name != "Quadro RTX 4000, Max-Q" || g[0].Load != 5 {
		t.Fatalf("%+v", g)
	}
}

func TestParseNvidiaPmon(t *testing.T) {
	out := `# gpu         pid   type     sm    mem    enc    dec    jpg    ofa     fb   ccpm    command
# Idx           #    C/G      %      %      %      %      %      %     MB     MB    name
    0       1234     G      5      2      -      -      -      -    120      0    Xorg
    0       5678     C     88     40      -      -      -      -   4096      0    python3
    1       5678     C     20      5      -      -      -      -   1024      0    python3
    0          -     -      -      -      -      -      -      -      -      -    -
`
	p := parseNvidiaPmon(out)
	if len(p) != 2 || p[1234].Pct != 5 || p[1234].Mem != 120<<20 || p[5678].Pct != 88 || p[5678].Mem != 5120<<20 {
		t.Fatalf("%+v", p)
	}
	// older drivers: no jpg/ofa/fb columns
	old := "# gpu        pid  type    sm   mem   enc   dec   command\n# Idx          #   C/G     %     %     %     %   name\n    0      42     G    12     3     -     -   Xorg\n"
	if p := parseNvidiaPmon(old); p[42].Pct != 12 || p[42].Mem != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestFdinfo(t *testing.T) {
	c := parseFdinfo("pos:\t0\nflags:\t02100002\ndrm-driver:\tamdgpu\ndrm-pdev:\t0000:03:00.0\ndrm-client-id:\t17\n" +
		"drm-memory-vram:\t524288 KiB\ndrm-memory-gtt:\t2048 KiB\ndrm-engine-gfx:\t1000000000 ns\ndrm-engine-compute:\t0 ns\n")
	if c == nil || c.driver != "amdgpu" || c.pdev != "0000:03:00.0" || c.id != "17" || c.mem != 512<<20 || c.busy["gfx"] != 1e9 {
		t.Fatalf("%+v", c)
	}
	if parseFdinfo("pos:\t0\nflags:\t0\n") != nil {
		t.Fatal("non-DRM fdinfo parsed")
	}
	t0 := time.Now()
	a := clientSample{at: t0, busy: map[string]uint64{"render": 1e9, "video": 0}}
	b := clientSample{at: t0.Add(2 * time.Second), busy: map[string]uint64{"render": 1.5e9, "video": 0.2e9}}
	if l := engineLoad(a, b); l != 25 {
		t.Fatal(l)
	}
	// xe reports cycles against the GPU's total cycles
	x := clientSample{at: t0, cyc: map[string]uint64{"rcs": 100}, total: map[string]uint64{"rcs": 1000}}
	y := clientSample{at: t0.Add(time.Second), cyc: map[string]uint64{"rcs": 400}, total: map[string]uint64{"rcs": 2000}}
	if l := engineLoad(x, y); l != 30 {
		t.Fatal(l)
	}
	tot := drmTotals(map[string]*drmClient{
		"a": {pdev: "p", busy: map[string]uint64{"render": 5}, cyc: map[string]uint64{}, total: map[string]uint64{}},
		"b": {pdev: "p", busy: map[string]uint64{"render": 7}, cyc: map[string]uint64{}, total: map[string]uint64{}},
	})
	if tot["p"].busy["render"] != 12 {
		t.Fatal(tot)
	}
}

func TestWindowsEngineUse(t *testing.T) {
	vals := map[string]float64{
		"pid_100_luid_0x00000000_0x0000D1A6_phys_0_eng_0_engtype_3D":          30,
		"pid_200_luid_0x00000000_0x0000D1A6_phys_0_eng_0_engtype_3D":          25,
		"pid_200_luid_0x00000000_0x0000D1A6_phys_0_eng_3_engtype_VideoDecode": 40,
		"pid_300_luid_0x00000000_0x0000AAAA_phys_0_eng_0_engtype_3D":          5,
	}
	ad, pr := engineUse(vals)
	if ad["0x00000000_0x0000d1a6"] != 55 || ad["0x00000000_0x0000aaaa"] != 5 || pr[200] != 40 || pr[100] != 30 {
		t.Fatal(ad, pr)
	}
}

func TestMacParsers(t *testing.T) {
	sp := `{"SPDisplaysDataType":[{"_name":"Apple M2 Pro","sppci_model":"Apple M2 Pro","sppci_vendor":"sppci_vendor_Apple","sppci_cores":"19"},
	{"_name":"AMD Radeon Pro 5500M","sppci_model":"AMD Radeon Pro 5500M","spdisplays_vram":"8 GB","sppci_vendor":"sppci_vendor_amd"}]}`
	c := parseSPDisplays(sp)
	if len(c) != 2 || c[0].vendor != "apple" || !c[0].shared || c[1].vendor != "amd" || c[1].vram != 8<<30 {
		t.Fatalf("%+v", c)
	}
	io := `+-o AGXAcceleratorG14X  <class AGXAcceleratorG14X, id 0x100000a3c, registered, matched, active, busy 0 (1 ms), retain 47>
    {
      "PerformanceStatistics" = {"In use system memory (driver)"=0,"Alloc system memory"=2004058112,"Tiled Scene Bytes"=0,"In use system memory"=345473024,"Device Utilization %"=14,"Renderer Utilization %"=12}
      "model" = "Apple M2 Pro"
    }
+-o AMDRadeonX6000_AMDNavi14GraphicsAccelerator  <class AMDRadeonX6000_AMDNavi14GraphicsAccelerator>
    {
      "PerformanceStatistics" = {"vramUsedBytes"=1073741824,"Device Utilization %"=3}
    }
`
	a := parseIoregAccel(io)
	if len(a) != 2 || a[0].load != 14 || a[0].mem != 345473024 || a[1].load != 3 || a[1].mem != 1<<30 {
		t.Fatalf("%+v", a)
	}
}

func TestDetectChassis(t *testing.T) {
	cases := []struct {
		inv     proto.Inventory
		smbios  []int
		battery bool
		want    string
	}{
		{proto.Inventory{Manufacturer: "VMware, Inc.", Model: "VMware7,1"}, []int{1}, false, "virtual"},
		{proto.Inventory{Virtual: "kvm"}, nil, false, "virtual"},
		{proto.Inventory{Manufacturer: "Dell Inc.", Model: "Latitude 7420"}, []int{10}, true, "laptop"},
		{proto.Inventory{Manufacturer: "Dell Inc.", Model: "OptiPlex 7090"}, []int{3}, false, "desktop"},
		{proto.Inventory{Manufacturer: "Dell Inc.", Model: "PowerEdge R740"}, []int{23}, false, "server"},
		{proto.Inventory{Platform: "Microsoft Windows Server 2022 Standard"}, []int{7}, false, "server"},
		{proto.Inventory{}, []int{2}, true, "laptop"},
		{proto.Inventory{}, nil, false, ""},
	}
	for i, c := range cases {
		if got := detectChassis(&c.inv, c.smbios, c.battery); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}
