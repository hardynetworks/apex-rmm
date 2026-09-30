//go:build linux

package agent

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// findX11 locates a running X server and its auth cookie by inspecting /proc.
func findX11() (display, xauth string, err error) {
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	sawWayland := false
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		base := filepath.Base(args[0])
		if base == "Xwayland" {
			sawWayland = true
			continue
		}
		if base != "Xorg" && base != "X" && !strings.HasSuffix(base, "Xorg.bin") && base != "Xvfb" && base != "Xvnc" {
			continue
		}
		d, a := "", ""
		for i, arg := range args[1:] {
			if strings.HasPrefix(arg, ":") && d == "" {
				d = arg
			}
			if arg == "-auth" && i+2 < len(args) {
				a = args[i+2]
			}
		}
		if d == "" {
			d = ":0"
		}
		return d, a, nil
	}
	// Fallback: any process environment exposing DISPLAY (e.g. startx without -auth).
	envs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, p := range envs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var d, a string
		for _, kv := range bytes.Split(b, []byte{0}) {
			if bytes.HasPrefix(kv, []byte("DISPLAY=")) {
				d = string(kv[8:])
			} else if bytes.HasPrefix(kv, []byte("XAUTHORITY=")) {
				a = string(kv[11:])
			}
		}
		if d != "" && !sawWayland {
			return d, a, nil
		}
	}
	if sawWayland {
		return "", "", errors.New("this device is running a Wayland session; built-in remote control needs Xorg (choose \"Ubuntu on Xorg\" at login or set WaylandEnable=false in /etc/gdm3/custom.conf). Use RustDesk as a fallback")
	}
	return "", "", errors.New("no graphical (X11) session found on this device; use the Terminal instead")
}

func spawnDesktopHelper(d proto.DesktopStart) error {
	display, xauth, err := findX11()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "desktop", "--url", d.URL, "--session", d.SessionID, "--token", d.Token)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	if xauth != "" {
		cmd.Env = append(cmd.Env, "XAUTHORITY="+xauth)
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
