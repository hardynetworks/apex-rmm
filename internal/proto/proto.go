// Package proto defines the messages exchanged between the Apex RMM server and agents.
package proto

import "encoding/json"

// Version is the protocol/agent version string. It is overridden at build time
// with -ldflags "-X github.com/hardynetworks/apex-rmm/internal/proto.Version=..."
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
	TypeEnergyConfig     = "energy_config" // EnergyConfig: how this device's power draw is measured
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
	Hostname      string   `json:"hostname"`
	OS            string   `json:"os"`       // windows, darwin, linux
	Platform      string   `json:"platform"` // e.g. "Microsoft Windows 11 Pro", "ubuntu"
	OSVersion     string   `json:"os_version"`
	KernelVersion string   `json:"kernel_version"`
	Arch          string   `json:"arch"`
	CPUModel      string   `json:"cpu_model"`
	CPUCores      int      `json:"cpu_cores"`
	CPUThreads    int      `json:"cpu_threads"`
	RAMTotal      uint64   `json:"ram_total"`
	Manufacturer  string   `json:"manufacturer,omitempty"`
	Model         string   `json:"model,omitempty"`
	Serial        string   `json:"serial,omitempty"`
	BootTime      int64    `json:"boot_time"`
	Disks         []Disk   `json:"disks"`
	NICs          []NIC    `json:"nics"`
	LoggedInUsers []string `json:"logged_in_users"`
	AgentVersion  string   `json:"agent_version"`
	Virtual       string   `json:"virtual,omitempty"`
	RustDeskID    string   `json:"rustdesk_id,omitempty"`
	Chassis       string   `json:"chassis,omitempty"` // server, desktop, laptop, virtual (detected; empty if unknown)
	GPUs          []string `json:"gpus,omitempty"`    // graphics adapter names
}

// Hello is sent right after the agent control socket connects.
type Hello struct {
	Inventory Inventory `json:"inventory"`
}

// Metrics is sent periodically.
type Metrics struct {
	CPU     float64  `json:"cpu"`
	Mem     float64  `json:"mem"`
	MemUsed uint64   `json:"mem_used"`
	Disk    float64  `json:"disk"` // highest disk usage percent
	Disks   []Disk   `json:"disks"`
	NetRx   uint64   `json:"net_rx"` // bytes/sec
	NetTx   uint64   `json:"net_tx"`
	Uptime  uint64   `json:"uptime"`
	Load1   float64  `json:"load1"`
	Procs   int      `json:"procs"`
	Users   []string `json:"users"`
	Energy  *Energy  `json:"energy,omitempty"`
	GPUs    []GPU    `json:"gpus,omitempty"`
}

// GPU is one graphics adapter's state at the time of a metrics sample.
// Values the adapter or driver doesn't report are -1 (or 0 for memory and clocks).
type GPU struct {
	Name     string  `json:"name"`
	Vendor   string  `json:"vendor"`              // nvidia, amd, intel, apple, other
	Load     float64 `json:"load"`                // % busy, -1 if unknown
	MemUsed  uint64  `json:"mem_used,omitempty"`  // bytes of video memory in use
	MemTotal uint64  `json:"mem_total,omitempty"` // bytes of dedicated video memory
	Shared   bool    `json:"shared,omitempty"`    // integrated GPU using system memory (MemTotal is not dedicated)
	TempC    float64 `json:"temp"`                // -1 if unknown
	FanPct   float64 `json:"fan"`                 // -1 if unknown
	ClockMHz float64 `json:"clock,omitempty"`     // graphics clock
	MemClock float64 `json:"mem_clock,omitempty"` // memory clock
	Watts    float64 `json:"w"`                   // board power, -1 if unknown
	Source   string  `json:"src"`                 // nvidia-smi, sysfs, pdh, ioreg
}

// Energy is the device's power draw at the time of a metrics sample.
type Energy struct {
	Watts  float64     `json:"w"`               // whole-device figure used for kWh (see Kind)
	Source string      `json:"src"`             // plug, ups, ipmi, meter, battery, telemetry, rapl, estimate
	Kind   string      `json:"kind"`            // measured (whole device), partial (components only), estimate
	Parts  []PowerPart `json:"parts,omitempty"` // components measured separately (CPU, RAM, GPU)
	UPS    *UPSStatus  `json:"ups,omitempty"`   // a UPS this device can see (NUT or apcupsd)
	Note   string      `json:"note,omitempty"`  // why the configured source isn't being used
}

// PowerPart is one measured component.
type PowerPart struct {
	Name   string  `json:"name"`
	Watts  float64 `json:"w"`
	Source string  `json:"src"`
}

// UPSStatus is read from a NUT or apcupsd server.
type UPSStatus struct {
	Name       string  `json:"name"`
	Model      string  `json:"model,omitempty"`
	Status     string  `json:"status"` // as reported, e.g. "OL", "OB LB", "ONLINE", "ONBATT"
	OnBattery  bool    `json:"on_battery"`
	Charge     float64 `json:"charge"`      // percent, -1 if unknown
	RuntimeSec int     `json:"runtime"`     // seconds left on battery, -1 if unknown
	LoadPct    float64 `json:"load"`        // percent of capacity, -1 if unknown
	Watts      float64 `json:"w"`           // output power, 0 if unknown
	NominalW   float64 `json:"nominal_w"`   // capacity in watts, 0 if unknown
	Estimated  bool    `json:"w_estimated"` // Watts worked out from load % and VA rating
}

// EnergyConfig is sent by the server when the agent connects and when it changes.
type EnergyConfig struct {
	Mode      string  `json:"mode"`       // auto (default), plug, ups, estimate, off
	PlugType  string  `json:"plug_type"`  // shelly, tasmota, kasa, json
	PlugAddr  string  `json:"plug_addr"`  // IP/host (or URL for json)
	PlugField string  `json:"plug_field"` // json: dotted path to the watts value
	UPSAddr   string  `json:"ups_addr"`   // NUT/apcupsd host[:port]; default this computer
	UPSName   string  `json:"ups_name"`   // NUT UPS name; default the first one
	UPSShare  float64 `json:"ups_share"`  // ups mode: percent of the UPS load that is this device (default 100)
	IdleW     float64 `json:"idle_w"`     // estimate: watts at idle (0 = guess)
	MaxW      float64 `json:"max_w"`      // estimate: watts at full CPU load (0 = guess)
	ExtraW    float64 `json:"extra_w"`    // added to partial readings and estimates (drives, fans, monitor…)
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
	GPU     float64 `json:"gpu,omitempty"`     // % of a GPU's time (highest engine), where measurable
	GPUMem  uint64  `json:"gpu_mem,omitempty"` // bytes of video memory
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
