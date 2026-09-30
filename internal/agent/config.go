// Package agent implements the Apex RMM endpoint agent.
package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// Config is persisted after enrollment.
type Config struct {
	Server   string `json:"server"`
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

// ServiceName is the OS service name.
const ServiceName = "apex-agent"

// InstallPath is where the agent binary lives once installed.
func InstallPath() string {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return filepath.Join(pf, "ApexRMM", "apex-agent.exe")
	case "darwin":
		return "/usr/local/apex-agent/apex-agent"
	default:
		return "/usr/local/bin/apex-agent"
	}
}

// ConfigPath is where credentials are stored (readable by root/SYSTEM only).
func ConfigPath() string {
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "ApexRMM", "agent.json")
	case "darwin":
		return "/Library/Application Support/ApexRMM/agent.json"
	default:
		return "/etc/apex-agent/agent.json"
	}
}

// LoadConfig reads the agent config.
func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, json.Unmarshal(b, &c)
}

// Save writes the agent config with restrictive permissions.
func (c *Config) Save() error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	return restrictFile(p)
}
