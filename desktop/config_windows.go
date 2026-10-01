//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	appName    = "Apex RMM"
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
)

type config struct {
	Server      string `json:"server"`       // e.g. https://rmm.example.com
	CaptureKeys *bool  `json:"capture_keys"` // send Win / Alt+Tab etc. to remote sessions (default on)
}

func (c *config) captureKeys() bool { return c.CaptureKeys == nil || *c.CaptureKeys }

func dataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	d := filepath.Join(base, "ApexRMM")
	_ = os.MkdirAll(d, 0o700)
	return d
}

func configPath() string { return filepath.Join(dataDir(), "desktop.json") }

func loadConfig() *config {
	c := &config{}
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, c)
	}
	return c
}

func (c *config) save() error {
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}

// normalizeServer turns user input into "https://host[:port][/path]" without a trailing slash.
func normalizeServer(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", errors.New("enter your Apex RMM server address")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("that doesn't look like a valid address")
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func origin(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

// ---- start with Windows (per-user Run key) ----

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appName)
	return err == nil
}

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err = k.DeleteValue(appName)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return err
	}
	return k.SetStringValue(appName, `"`+exePath()+`" --background`)
}
