// Package proto defines the messages exchanged between the Hardy RMM server and agents.
package proto

import "encoding/json"

// Version is the protocol/agent version string. It is overridden at build time
// with -ldflags "-X github.com/hardynetworks/hardy-rmm/internal/proto.Version=..."
var Version = "0.1.0"

// Envelope wraps every JSON message on the agent control WebSocket.
type Envelope struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Message types: agent -> server
const (
	TypeHello          = "hello"
	TypeMetrics        = "metrics"
	TypeResult         = "result"
	TypeScriptResult   = "script_result"
	TypeTerminalOutput = "terminal_output"
	TypeTerminalExit   = "terminal_exit"
	TypeLog            = "log"
)

// Message types: server -> agent
const (
	TypeRunScript        = "run_script"
	TypeTerminalOpen     = "terminal_open"
	TypeTerminalInput    = "terminal_input"
	TypeTerminalResize   = "terminal_resize"
	TypeTerminalClose    = "terminal_close"
	TypeDesktopStart     = "desktop_start"
	TypeListProcesses    = "list_processes"
	TypeKillProcess      = "kill_process"
	TypeListServices     = "list_services"
	TypeServiceAction    = "service_action"
	TypeListSoftware     = "list_software"
	TypePower            = "power"
	TypeRefreshInventory = "refresh_inventory"
	TypeUpdateAgent      = "update_agent"
	TypeUninstall        = "uninstall"
	TypeRustDesk         = "rustdesk_provision"
	TypePing             = "ping"
)

// EnrollRequest is POSTed by a new agent to /api/agent/enroll.
type EnrollRequest struct {
	Token     string    `json:"token"`
	Inventory Inventory `json:"inventory"`
}

// EnrollResponse returns the credentials an agent uses from then on.
type EnrollResponse struct {
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

// Disk describes one mounted volume.
type Disk struct {
	Mount   string  `json:"mount"`
	FSType  string  `json:"fstype"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

// NIC describes one network interface.
type NIC struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac"`
	Addrs []string `json:"addrs"`
}

// Inventory is the static-ish hardware/OS description of a device.
type Inventory struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`       // windows, darwin, linux
	Platform      string `json:"platform"` // e.g. "Microsoft Windows 11 Pro", "ubuntu"
	OSVersion     string `json:"os_version"`
	KernelVersion string `json:"kernel_version"`
	Arch          string `json:"arch"`
	CPUModel      string `json:"cpu_model"`
	CPUCores      int    `json:"cpu_cores"`
	CPUThreads    int    `json:"cpu_threads"`
	RAMTotal      uint64 `json:"ram_total"`
	Manufacturer  string `json:"manufacturer,omitempty"`
	Model         string `json:"model,omitempty"`
	Serial        string `json:"serial,omitempty"`
	BootTime      int64  `json:"boot_time"`
	Disks         []Disk `json:"disks"`
	NICs          []NIC  `json:"nics"`
	LoggedInUsers []string `json:"logged_in_users"`
	AgentVersion  string `json:"agent_version"`
	Virtual       string `json:"virtual,omitempty"`
	RustDeskID    string `json:"rustdesk_id,omitempty"`
}

// Hello is sent right after the agent control socket connects.
type Hello struct {
	Inventory Inventory `json:"inventory"`
}

// Metrics is sent periodically.
type Metrics struct {
	CPU       float64  `json:"cpu"`
	Mem       float64  `json:"mem"`
	MemUsed   uint64   `json:"mem_used"`
	Disk      float64  `json:"disk"` // highest disk usage percent
	Disks     []Disk   `json:"disks"`
	NetRx     uint64   `json:"net_rx"` // bytes/sec
	NetTx     uint64   `json:"net_tx"`
	Uptime    uint64   `json:"uptime"`
	Load1     float64  `json:"load1"`
	Procs     int      `json:"procs"`
	Users     []string `json:"users"`
}

// RunScript asks the agent to execute a script.
type RunScript struct {
	ResultID string            `json:"result_id"`
	Shell    string            `json:"shell"` // powershell, pwsh, cmd, bash, sh, zsh, python
	Body     string            `json:"body"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Timeout  int               `json:"timeout"` // seconds
}

// ScriptResult is sent when a script finishes.
type ScriptResult struct {
	ResultID string `json:"result_id"`
	Status   string `json:"status"` // success, failed, timeout, error
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
}

// TerminalOpen starts an interactive shell.
type TerminalOpen struct {
	SessionID string `json:"session_id"`
	Shell     string `json:"shell,omitempty"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// TerminalData carries terminal I/O (base64 in JSON because []byte).
type TerminalData struct {
	SessionID string `json:"session_id"`
	Data      []byte `json:"data,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// DesktopStart tells the agent to spawn a desktop helper that dials the server.
type DesktopStart struct {
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
	URL       string `json:"url"` // wss://.../api/agent/desktop
}

// Process is one running process.
type Process struct {
	PID     int32   `json:"pid"`
	Name    string  `json:"name"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	MemRSS  uint64  `json:"mem_rss"`
	Cmdline string  `json:"cmdline"`
}

// Service is one OS service.
type Service struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	StartType   string `json:"start_type"`
}

// Software is one installed application.
type Software struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	InstallDate string `json:"install_date,omitempty"`
}

// ServiceAction requests start/stop/restart of a service.
type ServiceAction struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// KillProcess requests termination of a process.
type KillProcess struct {
	PID int32 `json:"pid"`
}

// Power requests reboot or shutdown.
type Power struct {
	Action string `json:"action"` // reboot, shutdown
	Delay  int    `json:"delay"`
}

// RustDeskProvision asks the agent to install and configure RustDesk.
type RustDeskProvision struct {
	IDServer    string `json:"id_server"`
	RelayServer string `json:"relay_server"`
	Key         string `json:"key"`
	Password    string `json:"password"`
	ConfigB64   string `json:"config"` // RustDesk config string (reversed base64 JSON)
}

// RustDeskResult is returned after provisioning.
type RustDeskResult struct {
	ID     string `json:"id"`
	Output string `json:"output"`
}

// UpdateAgent tells the agent to self-update from the server.
type UpdateAgent struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Marshal builds an envelope with JSON data.
func Marshal(typ, id string, v any) ([]byte, error) {
	var raw json.RawMessage
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{Type: typ, ID: id, Data: raw})
}
