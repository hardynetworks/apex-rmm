package vsphere

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/vmware/govmomi/simulator"
)

// startSim runs a vCenter simulator (2 hosts' worth of default VPX model).
func startSim(t *testing.T) (Config, func()) {
	t.Helper()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.Listen = &url.URL{User: url.UserPassword("user", "pass")} // enforce credentials
	model.Service.TLS = new(tls.Config)                                     // serve HTTPS with a self-signed certificate
	s := model.Service.NewServer()
	cfg := Config{URL: s.URL.Host, Username: "user", Password: "pass", Insecure: true}
	return cfg, func() { s.Close(); model.Remove() }
}

func TestProbeAndCollect(t *testing.T) {
	cfg, stop := startSim(t)
	defer stop()
	ctx := context.Background()

	p, err := Probe(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hosts == 0 || p.VMs == 0 || p.Datastores == 0 {
		t.Fatalf("probe counted nothing: %+v", p)
	}

	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.IsVCenter() {
		t.Fatal("simulator should look like a vCenter")
	}
	inv, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != p.Hosts || len(inv.VMs) != p.VMs || len(inv.Datastores) != p.Datastores {
		t.Fatalf("collect %d/%d/%d != probe %d/%d/%d", len(inv.Hosts), len(inv.VMs), len(inv.Datastores), p.Hosts, p.VMs, p.Datastores)
	}
	h := inv.Hosts[0]
	if h.Name == "" || h.ConnectionState != "connected" || h.CPUCores == 0 || h.MemTotal == 0 {
		t.Fatalf("host looks empty: %+v", h)
	}
	var on int
	for _, v := range inv.VMs {
		if v.Name == "" || v.Ref == "" || v.UUID == "" {
			t.Fatalf("vm looks empty: %+v", v)
		}
		if v.PowerState == "poweredOn" {
			on++
		}
	}
	if on == 0 {
		t.Fatal("expected some powered-on VMs")
	}
	hostVMs := 0
	for _, h := range inv.Hosts {
		hostVMs += h.VMs
	}
	if hostVMs == 0 {
		t.Fatal("hosts report no VMs")
	}
	if d := inv.Datastores[0]; d.Capacity <= 0 || d.UsedPct() < 0 || d.UsedPct() > 100 {
		t.Fatalf("datastore looks wrong: %+v", d)
	}
}

func TestPowerActions(t *testing.T) {
	cfg, stop := startSim(t)
	defer stop()
	ctx := context.Background()
	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	inv, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, v := range inv.VMs {
		if v.PowerState == "poweredOn" {
			ref = v.Ref
			break
		}
	}
	if err := c.PowerVM(ctx, ref, "off"); err != nil {
		t.Fatal(err)
	}
	inv, _ = c.Collect(ctx)
	for _, v := range inv.VMs {
		if v.Ref == ref && v.PowerState != "poweredOff" {
			t.Fatalf("VM still %s after power off", v.PowerState)
		}
	}
	// powering off again is a clear, friendly error
	if err := c.PowerVM(ctx, ref, "off"); err == nil || !strings.Contains(err.Error(), "powered off") {
		t.Fatalf("want a friendly invalid-state error, got %v", err)
	}
	if err := c.PowerVM(ctx, ref, "on"); err != nil {
		t.Fatal(err)
	}
	if err := c.PowerVM(ctx, ref, "explode"); err == nil {
		t.Fatal("unknown action must be rejected")
	}
}

func TestBadCredentials(t *testing.T) {
	cfg, stop := startSim(t)
	defer stop()
	cfg.Password = "wrong"
	if _, err := Connect(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "sign-in failed") {
		t.Fatalf("want sign-in failure, got %v", err)
	}
}

func TestUntrustedCertificateReportsThumbprint(t *testing.T) {
	cfg, stop := startSim(t)
	defer stop()
	cfg.Insecure = false
	_, err := Connect(context.Background(), cfg)
	var ce *CertError
	if !errors.As(err, &ce) {
		t.Fatalf("want CertError, got %v", err)
	}
	if strings.Count(ce.Thumbprint, ":") != 19 {
		t.Fatalf("want a SHA-1 thumbprint, got %q", ce.Thumbprint)
	}
	// trusting exactly that thumbprint works, and a wrong one does not
	cfg.Thumbprint = ce.Thumbprint
	c, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pinned thumbprint should connect: %v", err)
	}
	c.Close()
	cfg.Thumbprint = strings.Repeat("00:", 19) + "00"
	if _, err := Connect(context.Background(), cfg); err == nil {
		t.Fatal("wrong thumbprint must be refused")
	}
}

func TestParseURL(t *testing.T) {
	for in, want := range map[string]string{
		"vc.lab":                    "https://vc.lab/sdk",
		"vc.lab:8443":               "https://vc.lab:8443/sdk",
		"https://vc.lab/":           "https://vc.lab/sdk",
		"https://vc.lab/ui/?x=1#y":  "https://vc.lab/sdk",
		"  https://user:p@vc.lab  ": "https://vc.lab/sdk",
	} {
		u, err := ParseURL(in)
		if err != nil || u.String() != want {
			t.Errorf("ParseURL(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://vc.lab", "https://"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) should fail", bad)
		}
	}
}

func TestOSFamily(t *testing.T) {
	for _, c := range []struct{ fam, id, name, want string }{
		{"windowsGuest", "", "", "windows"},
		{"linuxGuest", "", "", "linux"},
		{"", "windows2019srv_64Guest", "", "windows"},
		{"", "ubuntu64Guest", "Ubuntu Linux (64-bit)", "linux"},
		{"", "rhel8_64Guest", "", "linux"},
		{"", "darwin19_64Guest", "", "darwin"},
		{"", "otherGuest", "", "other"},
	} {
		if got := OSFamily(c.fam, c.id, c.name); got != c.want {
			t.Errorf("OSFamily(%q,%q,%q)=%s want %s", c.fam, c.id, c.name, got, c.want)
		}
	}
}

func TestInstallCommands(t *testing.T) {
	const u = "https://rmm.example.com/install/AbC-_123/windows.ps1"
	prog, args, err := WindowsInstallCommand(u)
	if err != nil || !strings.HasSuffix(prog, "powershell.exe") {
		t.Fatal(prog, err)
	}
	enc := args[strings.LastIndex(args, " ")+1:]
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw)%2 != 0 {
		t.Fatal("encoded command is not valid UTF-16LE base64")
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	script := string(utf16.Decode(u16))
	for _, want := range []string{u, "Tls12", `C:\Windows\Temp\apex-deploy.log`, "exit 1"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	_, args, err = LinuxInstallCommand("https://rmm.example.com/install/AbC-_123/linux.sh", true)
	if err != nil || !strings.HasPrefix(args, "-c '") || !strings.HasSuffix(args, "'") || !strings.Contains(args, "sudo -n sh $F") {
		t.Fatalf("sudo command wrong: %q %v", args, err)
	}
	if strings.Count(args, "'") != 2 {
		t.Fatalf("the command must contain exactly one quoted argument: %q", args)
	}
	_, args, _ = LinuxInstallCommand("https://rmm.example.com/install/x/linux.sh", false)
	if strings.Contains(args, "sudo") {
		t.Fatalf("root must not use sudo: %q", args)
	}

	// anything that could break out of the command line is refused
	for _, bad := range []string{
		"https://x.example.com/a;reboot", "https://x.example.com/a b", "https://x/'$(id)'",
		"https://x.example.com/a\"b", "file:///etc/passwd", "https://x.example.com/a`id`", "https://x.example.com/a|b",
	} {
		if _, _, err := LinuxInstallCommand(bad, false); err == nil {
			t.Errorf("linux accepted %q", bad)
		}
		if _, _, err := WindowsInstallCommand(bad); err == nil {
			t.Errorf("windows accepted %q", bad)
		}
	}
}

type fakeGuest struct {
	prog, args string
	exit       int32
	waitErr    error
	log        string
	deleted    string
}

func (f *fakeGuest) Start(_ context.Context, p, a string) (int64, error) {
	f.prog, f.args = p, a
	return 42, nil
}
func (f *fakeGuest) Wait(context.Context, int64) (int32, error) { return f.exit, f.waitErr }
func (f *fakeGuest) ReadFile(_ context.Context, path string, _ int64) (string, error) {
	return f.log, nil
}
func (f *fakeGuest) DeleteFile(_ context.Context, path string) error { f.deleted = path; return nil }

func TestRunInstall(t *testing.T) {
	ctx := context.Background()
	f := &fakeGuest{exit: 0, log: "Apex RMM agent installed.\n"}
	res, err := runInstall(ctx, f, "linux", "root", "https://rmm.example.com/install/t/linux.sh", time.Minute)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Log, "installed") {
		t.Fatalf("%+v %v", res, err)
	}
	if f.prog != "/bin/sh" || strings.Contains(f.args, "sudo") || f.deleted != "/tmp/apex-deploy.log" {
		t.Fatalf("root install wrong: %s %s deleted=%s", f.prog, f.args, f.deleted)
	}

	f = &fakeGuest{exit: 1, log: "curl: (22) The requested URL returned error: 404"}
	res, err = runInstall(ctx, f, "linux", "deploy", "https://rmm.example.com/install/t/linux.sh", time.Minute)
	if err != nil || res.ExitCode != 1 || !strings.Contains(res.Log, "404") || !strings.Contains(f.args, "sudo -n") {
		t.Fatalf("non-root failure wrong: %+v %v args=%s", res, err, f.args)
	}

	f = &fakeGuest{waitErr: errors.New("timed out"), log: "half done"}
	res, err = runInstall(ctx, f, "windows", "Administrator", "https://rmm.example.com/install/t/windows.ps1", time.Minute)
	if err == nil || res == nil || res.Log != "half done" || f.deleted != `C:\Windows\Temp\apex-deploy.log` {
		t.Fatalf("wait error should still return the log: %+v %v", res, err)
	}

	if _, err := runInstall(ctx, &fakeGuest{}, "darwin", "u", "https://rmm.example.com/x", time.Minute); !errors.Is(err, ErrUnsupportedGuest) {
		t.Fatalf("darwin should be unsupported, got %v", err)
	}
}

func TestSnapshotAndTail(t *testing.T) {
	if got := tail(strings.Repeat("a", 10), 4); got != "…aaaa" {
		t.Fatalf("tail = %q", got)
	}
	if got := tail("short", 100); got != "short" {
		t.Fatalf("tail = %q", got)
	}
}
