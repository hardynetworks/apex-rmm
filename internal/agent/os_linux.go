//go:build linux

package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hardynetworks/hardy-rmm/internal/proto"
)

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func hardwareInfo() (string, string, string) {
	return readTrim("/sys/class/dmi/id/sys_vendor"), readTrim("/sys/class/dmi/id/product_name"), readTrim("/sys/class/dmi/id/product_serial")
}

func windowsUsers() []string { return nil }

func listSoftware() ([]proto.Software, error) {
	var out []proto.Software
	if _, err := exec.LookPath("dpkg-query"); err == nil {
		s, err := runCmd(60*time.Second, "dpkg-query", "-W", "-f=${Package}\t${Version}\t${Maintainer}\t${db:Status-Abbrev}\n")
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(strings.NewReader(s))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) >= 4 && strings.HasPrefix(f[3], "ii") {
				out = append(out, proto.Software{Name: f[0], Version: f[1], Publisher: f[2]})
			}
		}
	} else if _, err := exec.LookPath("rpm"); err == nil {
		s, err := runCmd(60*time.Second, "rpm", "-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{VENDOR}\n")
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(strings.NewReader(s))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) >= 3 {
				out = append(out, proto.Software{Name: f[0], Version: f[1], Publisher: f[2]})
			}
		}
	} else if _, err := exec.LookPath("pacman"); err == nil {
		s, _ := runCmd(60*time.Second, "pacman", "-Q")
		for _, l := range strings.Split(s, "\n") {
			if f := strings.Fields(l); len(f) == 2 {
				out = append(out, proto.Software{Name: f[0], Version: f[1]})
			}
		}
	} else if _, err := exec.LookPath("apk"); err == nil {
		s, _ := runCmd(60*time.Second, "apk", "info", "-v")
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				i := strings.LastIndex(l, "-")
				if j := strings.LastIndex(l[:max(i, 0)], "-"); j > 0 {
					out = append(out, proto.Software{Name: l[:j], Version: l[j+1:]})
				}
			}
		}
	}
	if s, err := runCmd(30*time.Second, "snap", "list"); err == nil {
		for i, l := range strings.Split(s, "\n") {
			if f := strings.Fields(l); i > 0 && len(f) >= 5 {
				out = append(out, proto.Software{Name: f[0] + " (snap)", Version: f[1], Publisher: f[4]})
			}
		}
	}
	return out, nil
}

func listServices() ([]proto.Service, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, fmt.Errorf("systemd not available")
	}
	starts := map[string]string{}
	if s, err := runCmd(30*time.Second, "systemctl", "list-unit-files", "--type=service", "--no-legend", "--no-pager", "--plain"); err == nil {
		for _, l := range strings.Split(s, "\n") {
			if f := strings.Fields(l); len(f) >= 2 {
				starts[f[0]] = f[1]
			}
		}
	}
	s, err := runCmd(30*time.Second, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain")
	if err != nil {
		return nil, err
	}
	var out []proto.Service
	seen := map[string]bool{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		name := f[0]
		seen[name] = true
		out = append(out, proto.Service{Name: name, DisplayName: strings.Join(f[4:], " "), Status: f[3], StartType: starts[name]})
	}
	for name, st := range starts {
		if !seen[name] && !strings.Contains(name, "@") {
			out = append(out, proto.Service{Name: name, DisplayName: name, Status: "inactive", StartType: st})
		}
	}
	return out, nil
}

func serviceAction(name, action string) error {
	_, err := runCmd(60*time.Second, "systemctl", action, name)
	return err
}

func powerAction(action string, delay int) error {
	arg := "-r"
	if action == "shutdown" {
		arg = "-h"
	}
	when := "now"
	if delay > 60 {
		when = fmt.Sprintf("+%d", delay/60)
	}
	return exec.Command("shutdown", arg, when).Start()
}
