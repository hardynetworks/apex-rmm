//go:build darwin

package agent

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

var ioregSerial = regexp.MustCompile(`"IOPlatformSerialNumber" = "([^"]+)"`)

func hardwareInfo() (string, string, string) {
	model, _ := runCmd(5*time.Second, "sysctl", "-n", "hw.model")
	serial := ""
	if s, err := runCmd(10*time.Second, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice"); err == nil {
		if m := ioregSerial.FindStringSubmatch(s); m != nil {
			serial = m[1]
		}
	}
	return "Apple", strings.TrimSpace(model), serial
}

func windowsUsers() []string { return nil }

func listSoftware() ([]proto.Software, error) {
	s, err := runCmd(120*time.Second, "system_profiler", "SPApplicationsDataType", "-json", "-detailLevel", "mini")
	if err != nil {
		return nil, err
	}
	var data struct {
		Apps []struct {
			Name    string `json:"_name"`
			Version string `json:"version"`
			From    string `json:"obtained_from"`
			Path    string `json:"path"`
			Mod     string `json:"lastModified"`
		} `json:"SPApplicationsDataType"`
	}
	if err := json.Unmarshal([]byte(s), &data); err != nil {
		return nil, err
	}
	var out []proto.Software
	for _, a := range data.Apps {
		if strings.HasPrefix(a.Path, "/System/") {
			continue
		}
		out = append(out, proto.Software{Name: a.Name, Version: a.Version, Publisher: a.From, InstallDate: a.Mod})
	}
	return out, nil
}

func listServices() ([]proto.Service, error) {
	s, err := runCmd(30*time.Second, "launchctl", "list")
	if err != nil {
		return nil, err
	}
	var out []proto.Service
	for i, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if i == 0 || len(f) < 3 {
			continue
		}
		st := "stopped"
		if f[0] != "-" {
			st = "running"
		}
		out = append(out, proto.Service{Name: f[2], DisplayName: f[2], Status: st, StartType: "launchd"})
	}
	return out, nil
}

func serviceAction(name, action string) error {
	var err error
	switch action {
	case "start":
		_, err = runCmd(60*time.Second, "launchctl", "kickstart", "system/"+name)
	case "stop":
		_, err = runCmd(60*time.Second, "launchctl", "kill", "SIGTERM", "system/"+name)
	case "restart":
		_, err = runCmd(60*time.Second, "launchctl", "kickstart", "-k", "system/"+name)
	}
	return err
}

func powerAction(action string, delay int) error {
	arg := "-r"
	if action == "shutdown" {
		arg = "-h"
	}
	when := "now"
	if delay > 60 {
		when = "+" + itoa(delay/60)
	}
	return exec.Command("shutdown", arg, when).Start()
}
