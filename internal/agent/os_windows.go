//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/hardy-rmm/internal/proto"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	hwOnce                   sync.Once
	hwMfr, hwModel, hwSerial string
)

func hardwareInfo() (string, string, string) {
	hwOnce.Do(func() {
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE); err == nil {
			hwMfr, _, _ = k.GetStringValue("SystemManufacturer")
			hwModel, _, _ = k.GetStringValue("SystemProductName")
			k.Close()
		}
		if s, err := runCmd(20*time.Second, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-CimInstance Win32_BIOS).SerialNumber"); err == nil {
			hwSerial = strings.TrimSpace(s)
		}
	})
	return hwMfr, hwModel, hwSerial
}

// windowsUsers finds interactive users by the owners of explorer.exe.
func windowsUsers() []string {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, p := range procs {
		n, err := p.Name()
		if err != nil || !strings.EqualFold(n, "explorer.exe") {
			continue
		}
		if u, err := p.Username(); err == nil && u != "" {
			set[u] = true
		}
	}
	var out []string
	for u := range set {
		out = append(out, u)
	}
	return out
}

func listSoftware() ([]proto.Software, error) {
	seen := map[string]bool{}
	var out []proto.Software
	for _, path := range []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		for _, n := range names {
			sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, _ := sk.GetStringValue("DisplayName")
			ver, _, _ := sk.GetStringValue("DisplayVersion")
			pub, _, _ := sk.GetStringValue("Publisher")
			date, _, _ := sk.GetStringValue("InstallDate")
			sys, _, _ := sk.GetIntegerValue("SystemComponent")
			sk.Close()
			if name == "" || sys == 1 || seen[name+ver] {
				continue
			}
			seen[name+ver] = true
			out = append(out, proto.Software{Name: name, Version: ver, Publisher: pub, InstallDate: date})
		}
		k.Close()
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

var stateNames = map[svc.State]string{svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running", svc.ContinuePending: "continuing", svc.PausePending: "pausing", svc.Paused: "paused"}
var startNames = map[uint32]string{mgr.StartAutomatic: "automatic", mgr.StartManual: "manual", mgr.StartDisabled: "disabled", 0: "boot", 1: "system"}

func listServices() ([]proto.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()
	names, err := m.ListServices()
	if err != nil {
		return nil, err
	}
	var out []proto.Service
	for _, n := range names {
		s, err := m.OpenService(n)
		if err != nil {
			continue
		}
		cfg, err1 := s.Config()
		st, err2 := s.Query()
		s.Close()
		if err1 != nil || err2 != nil || cfg.ServiceType&windows.SERVICE_WIN32 == 0 {
			continue
		}
		start := startNames[cfg.StartType]
		if cfg.DelayedAutoStart && cfg.StartType == mgr.StartAutomatic {
			start = "automatic (delayed)"
		}
		out = append(out, proto.Service{Name: n, DisplayName: cfg.DisplayName, Status: stateNames[st.State], StartType: start})
	}
	return out, nil
}

func serviceAction(name, action string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	stop := func() error {
		st, err := s.Control(svc.Stop)
		if err != nil {
			return err
		}
		for i := 0; i < 60 && st.State != svc.Stopped; i++ {
			time.Sleep(500 * time.Millisecond)
			if st, err = s.Query(); err != nil {
				return err
			}
		}
		return nil
	}
	switch action {
	case "start":
		return s.Start()
	case "stop":
		return stop()
	case "restart":
		if err := stop(); err != nil {
			return err
		}
		return s.Start()
	}
	return fmt.Errorf("unknown action")
}

func powerAction(action string, delay int) error {
	arg := "/r"
	if action == "shutdown" {
		arg = "/s"
	}
	cmd := exec.Command("shutdown.exe", arg, "/f", "/t", fmt.Sprint(delay), "/c", "Hardy RMM: "+action+" requested by administrator")
	hideWindow(cmd)
	return cmd.Start()
}
