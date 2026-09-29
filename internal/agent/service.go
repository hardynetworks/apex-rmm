package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hardynetworks/hardy-rmm/internal/proto"
	"github.com/kardianos/service"
)

const systemdUnit = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
After=network-online.target
Wants=network-online.target

[Service]
StartLimitInterval=0
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
Restart=always
RestartSec=5
KillMode=process

[Install]
WantedBy=multi-user.target
`

func serviceConfig() *service.Config {
	return &service.Config{
		Name:        ServiceName,
		DisplayName: "Hardy RMM Agent",
		Description: "Hardy RMM remote monitoring and management agent",
		Executable:  InstallPath(),
		Arguments:   []string{"service"},
		Option: service.KeyValue{
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"OnFailureResetPeriod":   60,
			"DelayedAutoStart":       false,
			"SystemdScript":          systemdUnit,
			"KeepAlive":              true,
			"RunAtLoad":              true,
			"StartType":              "automatic",
		},
	}
}

type program struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(s service.Service) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("agent is not enrolled (%s): %w", ConfigPath(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		New(cfg).Run(ctx)
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

// RunService runs the agent under the OS service manager.
func RunService() error {
	s, err := service.New(&program{}, serviceConfig())
	if err != nil {
		return err
	}
	return s.Run()
}

// RunForeground runs the agent in the console (for debugging).
func RunForeground(ctx context.Context) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("not enrolled: %w", err)
	}
	New(cfg).Run(ctx)
	return nil
}

// Install enrolls the device, copies the binary into place and installs the service.
func Install(server, token string, force bool) error {
	if !isAdmin() {
		return errors.New("install must be run as root / Administrator")
	}
	server = strings.TrimRight(server, "/")
	if server == "" {
		return errors.New("--server is required")
	}
	// 1. copy binary
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dst := InstallPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	s, err := service.New(&program{}, serviceConfig())
	if err != nil {
		return err
	}
	if st, err := s.Status(); err == nil && st == service.StatusRunning {
		_ = s.Stop()
		time.Sleep(2 * time.Second)
	}
	if !samePath(self, dst) {
		if err := copyFile(self, dst); err != nil {
			return fmt.Errorf("copy agent to %s: %w", dst, err)
		}
	}
	// 2. enroll unless already enrolled against this server
	cfg, err := LoadConfig()
	if err != nil || force || cfg.Server != server || cfg.DeviceID == "" {
		if token == "" {
			return errors.New("--token is required for a new enrollment")
		}
		fmt.Println("Enrolling with", server, "...")
		inv := CollectInventory()
		body, _ := json.Marshal(proto.EnrollRequest{Token: token, Inventory: inv})
		req, _ := http.NewRequest(http.MethodPost, server+"/api/agent/enroll", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient().Do(req)
		if err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return fmt.Errorf("enroll failed: %s %s", resp.Status, strings.TrimSpace(string(rb)))
		}
		var er proto.EnrollResponse
		if err := json.Unmarshal(rb, &er); err != nil {
			return err
		}
		cfg = &Config{Server: server, DeviceID: er.DeviceID, Secret: er.Secret}
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Println("Enrolled as device", er.DeviceID)
	} else {
		fmt.Println("Already enrolled as", cfg.DeviceID, "- reinstalling service")
	}
	// 3. service
	_ = s.Uninstall()
	if err := s.Install(); err != nil {
		return fmt.Errorf("install service: %w", err)
	}
	postInstall()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Println("Hardy RMM agent installed and running.")
	return nil
}

// Uninstall removes the service, config and binary.
func Uninstall() error {
	s, err := service.New(&program{}, serviceConfig())
	if err != nil {
		return err
	}
	_ = s.Stop()
	_ = s.Uninstall()
	_ = os.Remove(ConfigPath())
	if runtime.GOOS != "windows" {
		_ = os.Remove(InstallPath())
	}
	fmt.Println("Hardy RMM agent removed.")
	return nil
}

// selfUninstall is triggered remotely when a device is deleted from the dashboard.
func selfUninstall() {
	log.Println("uninstall requested by server")
	_ = os.Remove(ConfigPath())
	if runtime.GOOS == "windows" {
		dir := filepath.Dir(InstallPath())
		cmd := exec.Command("cmd.exe", "/c", fmt.Sprintf(`ping -n 4 127.0.0.1 >nul & sc.exe stop %s & ping -n 4 127.0.0.1 >nul & sc.exe delete %s & rmdir /s /q "%s"`, ServiceName, ServiceName, dir))
		detach(cmd)
		_ = cmd.Start()
		return
	}
	_ = os.Remove(InstallPath())
	if s, err := service.New(&program{}, serviceConfig()); err == nil {
		_ = s.Uninstall()
		go func() {
			time.Sleep(time.Second)
			_ = s.Stop()
			os.Exit(0)
		}()
	}
}

func samePath(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	if ra == "" {
		ra = a
	}
	if rb == "" {
		rb = b
	}
	return strings.EqualFold(filepath.Clean(ra), filepath.Clean(rb))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// a running exe can be renamed but not overwritten
		_ = os.Remove(dst + ".old")
		_ = os.Rename(dst, dst+".old")
	}
	return os.Rename(tmp, dst)
}
