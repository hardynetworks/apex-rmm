package vsphere

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Sensor is a hardware sensor or hardware element reported by an ESXi host.
type Sensor struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	State   string `json:"state"` // green | yellow | red | unknown
	Reading string `json:"reading,omitempty"`
}

// Reading is a numeric hardware sensor value (temperature, fan speed, PSU, voltage ...).
type Reading struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"` // temperature | fan | power | voltage | current | other
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`  // C | RPM | W | V | A | ...
	State string  `json:"state"` // green | yellow | red | unknown
}

type Host struct {
	Ref             string     `json:"ref"`
	Name            string     `json:"name"`
	Cluster         string     `json:"cluster,omitempty"`
	ConnectionState string     `json:"connection_state"` // connected | disconnected | notResponding
	PowerState      string     `json:"power_state"`
	Maintenance     bool       `json:"maintenance"`
	Status          string     `json:"status"` // overall: green | yellow | red | gray
	Version         string     `json:"version"`
	Build           string     `json:"build"`
	Vendor          string     `json:"vendor"`
	Model           string     `json:"model"`
	CPUModel        string     `json:"cpu_model"`
	CPUMhz          int32      `json:"cpu_mhz"` // per core
	CPUCores        int16      `json:"cpu_cores"`
	CPUThreads      int16      `json:"cpu_threads"`
	MemTotal        int64      `json:"mem_total"` // bytes
	CPUUsedMhz      int32      `json:"cpu_used_mhz"`
	MemUsedMB       int32      `json:"mem_used_mb"`
	UptimeSec       int32      `json:"uptime_sec"`
	BootTime        *time.Time `json:"boot_time,omitempty"`
	VMs             int        `json:"vms"`
	VMsOn           int        `json:"vms_on"`
	Sensors         []Sensor   `json:"sensors,omitempty"` // only those not green
	Readings        []Reading  `json:"readings,omitempty"`
	PowerWatts      *float64   `json:"power_watts,omitempty"`     // current draw reported by the host (performance counter)
	PowerCapWatts   *float64   `json:"power_cap_watts,omitempty"` // configured power cap, if any
}

// MaxTempC is the hottest temperature sensor, and whether there is one.
func (h Host) MaxTempC() (float64, bool) {
	best, ok := 0.0, false
	for _, r := range h.Readings {
		if r.Kind == "temperature" && (!ok || r.Value > best) {
			best, ok = r.Value, true
		}
	}
	return best, ok
}

// FanSummary returns the number of fans with a reading and the slowest speed.
func (h Host) FanSummary() (count int, minRPM float64) {
	for _, r := range h.Readings {
		if r.Kind == "fan" {
			if count == 0 || r.Value < minRPM {
				minRPM = r.Value
			}
			count++
		}
	}
	return
}

// CPUPct / MemPct are derived utilisation percentages (0-100).
func (h Host) CPUPct() float64 {
	total := float64(h.CPUMhz) * float64(h.CPUCores)
	if total <= 0 {
		return 0
	}
	return clampPct(float64(h.CPUUsedMhz) / total * 100)
}

func (h Host) MemPct() float64 {
	if h.MemTotal <= 0 {
		return 0
	}
	return clampPct(float64(h.MemUsedMB) * 1024 * 1024 / float64(h.MemTotal) * 100)
}

func clampPct(v float64) float64 { return math.Max(0, math.Min(100, v)) }

type VM struct {
	Ref             string     `json:"ref"`
	Name            string     `json:"name"`
	UUID            string     `json:"uuid"`          // BIOS UUID
	InstanceUUID    string     `json:"instance_uuid"` // vCenter instance UUID
	HostRef         string     `json:"host_ref"`
	HostName        string     `json:"host_name,omitempty"`
	PowerState      string     `json:"power_state"` // poweredOn | poweredOff | suspended
	Template        bool       `json:"template"`
	GuestID         string     `json:"guest_id"`
	GuestOS         string     `json:"guest_os"`
	OSFamily        string     `json:"os_family"` // windows | linux | darwin | other
	Hostname        string     `json:"hostname"`
	IPs             []string   `json:"ips"`
	ToolsRunning    string     `json:"tools_running"` // guestToolsRunning | guestToolsNotRunning | guestToolsExecutingScripts
	ToolsVersion    string     `json:"tools_version"` // guestToolsCurrent | guestToolsNeedUpgrade | guestToolsNotInstalled ...
	CPUs            int32      `json:"cpus"`
	MemMB           int32      `json:"mem_mb"`
	DiskUsed        int64      `json:"disk_used"`
	DiskProvisioned int64      `json:"disk_provisioned"`
	CPUUsedMhz      int32      `json:"cpu_used_mhz"`
	GuestMemMB      int32      `json:"guest_mem_mb"`
	Snapshots       int        `json:"snapshots"`
	OldestSnapshot  *time.Time `json:"oldest_snapshot,omitempty"`
	Status          string     `json:"status"`
	Annotation      string     `json:"annotation,omitempty"`
	BootTime        *time.Time `json:"boot_time,omitempty"`
}

// ToolsOK reports whether VMware Tools is running (needed for guest operations).
func (v VM) ToolsOK() bool {
	return v.ToolsRunning == "guestToolsRunning" || v.ToolsRunning == "guestToolsExecutingScripts"
}

type Datastore struct {
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Capacity    int64  `json:"capacity"`
	Free        int64  `json:"free"`
	Accessible  bool   `json:"accessible"`
	Maintenance string `json:"maintenance"` // normal | inMaintenance | enteringMaintenance
	Status      string `json:"status"`
}

func (d Datastore) UsedPct() float64 {
	if d.Capacity <= 0 {
		return 0
	}
	return clampPct(float64(d.Capacity-d.Free) / float64(d.Capacity) * 100)
}

// Alarm is a triggered vCenter alarm on an inventory object.
type Alarm struct {
	EntityRef    string    `json:"entity_ref"`
	EntityType   string    `json:"entity_type"`
	EntityName   string    `json:"entity_name"`
	AlarmRef     string    `json:"alarm_ref"`
	AlarmName    string    `json:"alarm_name"`
	Status       string    `json:"status"` // red | yellow
	Time         time.Time `json:"time"`
	Acknowledged bool      `json:"acknowledged"`
}

// Inventory is a point-in-time snapshot of the environment.
type Inventory struct {
	About       About       `json:"about"`
	CollectedAt time.Time   `json:"collected_at"`
	Hosts       []Host      `json:"hosts"`
	VMs         []VM        `json:"vms"`
	Datastores  []Datastore `json:"datastores"`
	Alarms      []Alarm     `json:"alarms"`
}

// OSFamily classifies a guest from VMware's guestFamily / guestId strings.
func OSFamily(guestFamily, guestID, guestFullName string) string {
	switch guestFamily {
	case "windowsGuest":
		return "windows"
	case "linuxGuest":
		return "linux"
	case "darwinGuestFamily":
		return "darwin"
	}
	id := strings.ToLower(guestID + " " + guestFullName)
	switch {
	case strings.Contains(id, "windows"):
		return "windows"
	case strings.Contains(id, "darwin") || strings.Contains(id, "macos") || strings.Contains(id, "mac os"):
		return "darwin"
	}
	for _, k := range []string{"linux", "ubuntu", "debian", "centos", "rhel", "red hat", "sles", "suse", "fedora", "oracle", "rocky", "alma", "photon", "amazon", "alpine", "coreos", "flatcar", "arch"} {
		if strings.Contains(id, k) {
			return "linux"
		}
	}
	return "other"
}

func str(s types.ManagedEntityStatus) string { return string(s) }

// Collect gathers hosts, VMs, datastores and triggered alarms.
func (c *Conn) Collect(ctx context.Context) (*Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	vc := c.c.Client
	mgr := view.NewManager(vc)
	v, err := mgr.CreateContainerView(ctx, vc.ServiceContent.RootFolder,
		[]string{"HostSystem", "VirtualMachine", "Datastore", "ClusterComputeResource"}, true)
	if err != nil {
		return nil, fmt.Errorf("create inventory view: %w", cleanErr(err))
	}
	defer v.Destroy(context.Background())

	var hosts []mo.HostSystem
	if err := v.Retrieve(ctx, []string{"HostSystem"}, []string{
		"name", "parent", "runtime", "summary.hardware", "summary.quickStats", "summary.overallStatus",
		"summary.config.product", "triggeredAlarmState",
	}, &hosts); err != nil {
		return nil, fmt.Errorf("read hosts: %w", cleanErr(err))
	}
	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, []string{
		"name", "config.uuid", "config.instanceUuid", "config.template", "config.annotation", "config.guestId", "config.guestFullName",
		"config.hardware.numCPU", "config.hardware.memoryMB", "runtime.powerState", "runtime.host", "runtime.bootTime",
		"guest.guestFullName", "guest.guestFamily", "guest.hostName", "guest.ipAddress", "guest.net",
		"guest.toolsRunningStatus", "guest.toolsVersionStatus2",
		"summary.storage", "summary.quickStats", "summary.overallStatus", "snapshot", "triggeredAlarmState",
	}, &vms); err != nil {
		return nil, fmt.Errorf("read virtual machines: %w", cleanErr(err))
	}
	var dss []mo.Datastore
	if err := v.Retrieve(ctx, []string{"Datastore"}, []string{"name", "summary", "triggeredAlarmState"}, &dss); err != nil {
		return nil, fmt.Errorf("read datastores: %w", cleanErr(err))
	}
	var clusters []mo.ClusterComputeResource
	_ = v.Retrieve(ctx, []string{"ClusterComputeResource"}, []string{"name", "triggeredAlarmState"}, &clusters)

	clusterName := map[string]string{}
	for _, cl := range clusters {
		clusterName[cl.Self.Value] = cl.Name
	}

	inv := &Inventory{About: c.About(), CollectedAt: time.Now().UTC()}
	hostName := map[string]string{}

	for _, h := range hosts {
		hostName[h.Self.Value] = h.Name
		out := Host{
			Ref: h.Self.Value, Name: h.Name,
			ConnectionState: string(h.Runtime.ConnectionState), PowerState: string(h.Runtime.PowerState),
			Maintenance: h.Runtime.InMaintenanceMode, Status: str(h.Summary.OverallStatus),
			BootTime: h.Runtime.BootTime,
		}
		if h.Parent != nil {
			out.Cluster = clusterName[h.Parent.Value]
		}
		hw := h.Summary.Hardware
		if hw != nil {
			out.Vendor, out.Model, out.CPUModel = hw.Vendor, hw.Model, hw.CpuModel
			out.CPUMhz, out.CPUCores, out.CPUThreads, out.MemTotal = hw.CpuMhz, hw.NumCpuCores, hw.NumCpuThreads, hw.MemorySize
		}
		qs := h.Summary.QuickStats
		if qs.OverallCpuUsage > 0 {
			out.CPUUsedMhz = qs.OverallCpuUsage
		}
		if qs.OverallMemoryUsage > 0 {
			out.MemUsedMB = qs.OverallMemoryUsage
		}
		out.UptimeSec = qs.Uptime
		if p := h.Summary.Config.Product; p != nil {
			out.Version, out.Build = p.Version, p.Build
		}
		out.Sensors = hostSensors(h.Runtime)
		out.Readings = hostReadings(h.Runtime)
		inv.Hosts = append(inv.Hosts, out)
		inv.Alarms = append(inv.Alarms, alarmsOf("host", h.Self.Value, h.Name, h.TriggeredAlarmState)...)
	}

	c.addHostPower(ctx, inv.Hosts)

	for _, vm := range vms {
		out := VM{Ref: vm.Self.Value, Name: vm.Name, Status: str(vm.Summary.OverallStatus), PowerState: string(vm.Runtime.PowerState), BootTime: vm.Runtime.BootTime}
		if vm.Config != nil {
			out.UUID, out.InstanceUUID, out.Template = vm.Config.Uuid, vm.Config.InstanceUuid, vm.Config.Template
			out.Annotation, out.GuestID, out.GuestOS = vm.Config.Annotation, vm.Config.GuestId, vm.Config.GuestFullName
			out.CPUs, out.MemMB = vm.Config.Hardware.NumCPU, vm.Config.Hardware.MemoryMB
		}
		family := ""
		if g := vm.Guest; g != nil {
			if g.GuestFullName != "" {
				out.GuestOS = g.GuestFullName
			}
			family = g.GuestFamily
			out.Hostname = g.HostName
			out.ToolsRunning, out.ToolsVersion = g.ToolsRunningStatus, g.ToolsVersionStatus2
			out.IPs = guestIPs(g)
		}
		out.OSFamily = OSFamily(family, out.GuestID, out.GuestOS)
		if vm.Runtime.Host != nil {
			out.HostRef = vm.Runtime.Host.Value
			out.HostName = hostName[out.HostRef]
		}
		if st := vm.Summary.Storage; st != nil {
			out.DiskUsed = st.Committed
			out.DiskProvisioned = st.Committed + st.Uncommitted
		}
		out.CPUUsedMhz = vm.Summary.QuickStats.OverallCpuUsage
		out.GuestMemMB = vm.Summary.QuickStats.GuestMemoryUsage
		if vm.Snapshot != nil {
			out.Snapshots, out.OldestSnapshot = walkSnapshots(vm.Snapshot.RootSnapshotList)
		}
		inv.VMs = append(inv.VMs, out)
		if !out.Template {
			inv.Alarms = append(inv.Alarms, alarmsOf("vm", vm.Self.Value, vm.Name, vm.TriggeredAlarmState)...)
		}
	}
	// per-host VM counts
	count := map[string][2]int{}
	for _, vm := range inv.VMs {
		if vm.Template || vm.HostRef == "" {
			continue
		}
		c := count[vm.HostRef]
		c[0]++
		if vm.PowerState == "poweredOn" {
			c[1]++
		}
		count[vm.HostRef] = c
	}
	for i := range inv.Hosts {
		c := count[inv.Hosts[i].Ref]
		inv.Hosts[i].VMs, inv.Hosts[i].VMsOn = c[0], c[1]
	}

	for _, ds := range dss {
		s := ds.Summary
		out := Datastore{Ref: ds.Self.Value, Name: ds.Name, Type: s.Type, URL: s.Url, Capacity: s.Capacity, Free: s.FreeSpace,
			Accessible: s.Accessible, Maintenance: s.MaintenanceMode, Status: str(ds.OverallStatus)}
		inv.Datastores = append(inv.Datastores, out)
		inv.Alarms = append(inv.Alarms, alarmsOf("datastore", ds.Self.Value, ds.Name, ds.TriggeredAlarmState)...)
	}
	for _, cl := range clusters {
		inv.Alarms = append(inv.Alarms, alarmsOf("cluster", cl.Self.Value, cl.Name, cl.TriggeredAlarmState)...)
	}

	c.nameAlarms(ctx, inv)

	sort.Slice(inv.Hosts, func(i, j int) bool { return strings.ToLower(inv.Hosts[i].Name) < strings.ToLower(inv.Hosts[j].Name) })
	sort.Slice(inv.VMs, func(i, j int) bool { return strings.ToLower(inv.VMs[i].Name) < strings.ToLower(inv.VMs[j].Name) })
	sort.Slice(inv.Datastores, func(i, j int) bool {
		return strings.ToLower(inv.Datastores[i].Name) < strings.ToLower(inv.Datastores[j].Name)
	})
	return inv, nil
}

func guestIPs(g *types.GuestInfo) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ip string) {
		if ip == "" || seen[ip] || strings.HasPrefix(ip, "fe80") || strings.HasPrefix(ip, "169.254.") || strings.Contains(ip, ":") {
			return // IPv4 only: link-local and IPv6 are noise for matching
		}
		seen[ip] = true
		out = append(out, ip)
	}
	add(g.IpAddress)
	for _, n := range g.Net {
		for _, ip := range n.IpAddress {
			add(ip)
		}
	}
	return out
}

func walkSnapshots(list []types.VirtualMachineSnapshotTree) (int, *time.Time) {
	n := 0
	var oldest *time.Time
	var walk func([]types.VirtualMachineSnapshotTree)
	walk = func(l []types.VirtualMachineSnapshotTree) {
		for _, s := range l {
			n++
			if oldest == nil || s.CreateTime.Before(*oldest) {
				t := s.CreateTime
				oldest = &t
			}
			walk(s.ChildSnapshotList)
		}
	}
	walk(list)
	return n, oldest
}

func alarmsOf(kind, ref, name string, states []types.AlarmState) []Alarm {
	var out []Alarm
	for _, a := range states {
		st := string(a.OverallStatus)
		if st != "red" && st != "yellow" {
			continue
		}
		ack := a.Acknowledged != nil && *a.Acknowledged
		out = append(out, Alarm{EntityRef: ref, EntityType: kind, EntityName: name, AlarmRef: a.Alarm.Value, Status: st, Time: a.Time, Acknowledged: ack})
	}
	return out
}

// nameAlarms resolves alarm definition names.
func (c *Conn) nameAlarms(ctx context.Context, inv *Inventory) {
	if len(inv.Alarms) == 0 {
		return
	}
	seen := map[string]bool{}
	var refs []types.ManagedObjectReference
	for _, a := range inv.Alarms {
		if !seen[a.AlarmRef] {
			seen[a.AlarmRef] = true
			refs = append(refs, types.ManagedObjectReference{Type: "Alarm", Value: a.AlarmRef})
		}
	}
	var defs []mo.Alarm
	if err := property.DefaultCollector(c.c.Client).Retrieve(ctx, refs, []string{"info.name"}, &defs); err != nil {
		return // names are cosmetic; keep the refs
	}
	names := map[string]string{}
	for _, d := range defs {
		names[d.Self.Value] = d.Info.Name
	}
	for i := range inv.Alarms {
		if n := names[inv.Alarms[i].AlarmRef]; n != "" {
			inv.Alarms[i].AlarmName = n
		} else {
			inv.Alarms[i].AlarmName = inv.Alarms[i].AlarmRef
		}
	}
}

// hostSensors returns hardware sensors that are not healthy.
func hostSensors(rt types.HostRuntimeInfo) []Sensor {
	var out []Sensor
	hs := rt.HealthSystemRuntime
	if hs == nil {
		return nil
	}
	state := func(d types.BaseElementDescription) string {
		if d == nil {
			return "unknown"
		}
		return strings.ToLower(d.GetElementDescription().Key)
	}
	if si := hs.SystemHealthInfo; si != nil {
		for _, n := range si.NumericSensorInfo {
			st := state(n.HealthState)
			if st == "green" || st == "unknown" || st == "" {
				continue
			}
			val := float64(n.CurrentReading) * math.Pow(10, float64(n.UnitModifier))
			out = append(out, Sensor{Name: n.Name, Type: n.SensorType, State: st, Reading: fmt.Sprintf("%.1f %s", val, n.BaseUnits)})
		}
	}
	if hw := hs.HardwareStatusInfo; hw != nil {
		add := func(kind string, els []types.BaseHostHardwareElementInfo) {
			for _, e := range els {
				info := e.GetHostHardwareElementInfo()
				st := state(info.Status)
				if st == "green" || st == "unknown" || st == "" {
					continue
				}
				out = append(out, Sensor{Name: info.Name, Type: kind, State: st})
			}
		}
		add("cpu", hw.CpuStatusInfo)
		add("memory", hw.MemoryStatusInfo)
		for _, e := range hw.StorageStatusInfo {
			st := state(e.Status)
			if st == "green" || st == "unknown" || st == "" {
				continue
			}
			out = append(out, Sensor{Name: e.Name, Type: "storage", State: st})
		}
	}
	return out
}

// hostReadings returns every numeric sensor the host exposes, scaled to real units.
func hostReadings(rt types.HostRuntimeInfo) []Reading {
	hs := rt.HealthSystemRuntime
	if hs == nil || hs.SystemHealthInfo == nil {
		return nil
	}
	var out []Reading
	for _, n := range hs.SystemHealthInfo.NumericSensorInfo {
		if n.CurrentReading == 0 && n.SensorType == "" {
			continue
		}
		val := float64(n.CurrentReading) * math.Pow(10, float64(n.UnitModifier))
		kind, unit := sensorKind(n.SensorType, n.BaseUnits)
		if kind == "other" && n.BaseUnits == "" {
			continue
		}
		st := "unknown"
		if n.HealthState != nil {
			st = strings.ToLower(n.HealthState.GetElementDescription().Key)
		}
		out = append(out, Reading{Name: n.Name, Kind: kind, Value: math.Round(val*10) / 10, Unit: unit, State: st})
		if len(out) >= 120 {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// sensorKind maps vSphere's sensor type and base units to a kind and a short unit.
func sensorKind(sensorType, baseUnits string) (kind, unit string) {
	u := strings.ToLower(baseUnits)
	switch {
	case strings.Contains(u, "degrees c"), u == "c", u == "celsius":
		return "temperature", "C"
	case strings.Contains(u, "degrees f"):
		return "temperature", "F"
	case u == "rpm":
		return "fan", "RPM"
	case u == "watts" || u == "w":
		return "power", "W"
	case u == "volts" || u == "v":
		return "voltage", "V"
	case u == "amps" || u == "a":
		return "current", "A"
	}
	switch strings.ToLower(sensorType) {
	case "temperature":
		return "temperature", baseUnits
	case "fan":
		return "fan", baseUnits
	case "power":
		return "power", baseUnits
	case "voltage":
		return "voltage", baseUnits
	}
	return "other", baseUnits
}

// addHostPower fills in each host's current power draw (and cap) from the
// real-time performance counters. Hosts or vCenters that do not expose them are
// left alone: this is best effort and never fails a collection.
func (c *Conn) addHostPower(ctx context.Context, hosts []Host) {
	if len(hosts) == 0 {
		return
	}
	refs := make([]types.ManagedObjectReference, 0, len(hosts))
	byRef := map[string]int{}
	for i, h := range hosts {
		if h.ConnectionState != "connected" {
			continue
		}
		refs = append(refs, types.ManagedObjectReference{Type: "HostSystem", Value: h.Ref})
		byRef[h.Ref] = i
	}
	if len(refs) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pm := performance.NewManager(c.c.Client)
	spec := types.PerfQuerySpec{MaxSample: 1, IntervalId: 20}
	samples, err := pm.SampleByName(pctx, spec, []string{"power.power.average", "power.powerCap.average"}, refs)
	if err != nil {
		return
	}
	series, err := pm.ToMetricSeries(pctx, samples)
	if err != nil {
		return
	}
	for _, em := range series {
		i, ok := byRef[em.Entity.Value]
		if !ok {
			continue
		}
		for _, v := range em.Value {
			if len(v.Value) == 0 {
				continue
			}
			w := float64(v.Value[len(v.Value)-1])
			if w < 0 {
				continue
			}
			switch v.Name {
			case "power.power.average":
				hosts[i].PowerWatts = &w
			case "power.powerCap.average":
				if w > 0 {
					hosts[i].PowerCapWatts = &w
				}
			}
		}
	}
}
