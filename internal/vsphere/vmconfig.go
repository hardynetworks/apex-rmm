package vsphere

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// VM configuration: read a VM's hardware, networks it can use, datastores and snapshots, and
// change them (CPU / memory, disks, network adapters, CD/DVD, name and notes, snapshots).

// VMDisk is a virtual disk.
type VMDisk struct {
	Key       int32  `json:"key"`
	Label     string `json:"label"`
	SizeBytes int64  `json:"size_bytes"`
	File      string `json:"file"`
	Datastore string `json:"datastore"`
	Thin      bool   `json:"thin"`
	Unit      string `json:"unit"` // controller:unit, e.g. "SCSI 0:1"
}

// VMNic is a network adapter.
type VMNic struct {
	Key       int32  `json:"key"`
	Label     string `json:"label"`
	Type      string `json:"type"` // vmxnet3, e1000e…
	MAC       string `json:"mac"`
	Network   string `json:"network"`     // network / port group name
	NetRef    string `json:"network_ref"` // its managed object reference
	Connected bool   `json:"connected"`
	OnBoot    bool   `json:"start_connected"`
}

// VMCdrom is a CD/DVD drive.
type VMCdrom struct {
	Key       int32  `json:"key"`
	Label     string `json:"label"`
	ISO       string `json:"iso"` // "[datastore] path/file.iso" when an ISO is mounted
	Connected bool   `json:"connected"`
	OnBoot    bool   `json:"start_connected"`
}

// VMSnapshot is one snapshot, flattened from the tree (Parent is its parent's ref).
type VMSnapshot struct {
	Ref         string    `json:"ref"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Created     time.Time `json:"created"`
	Parent      string    `json:"parent,omitempty"`
	Depth       int       `json:"depth"`
	Current     bool      `json:"current"`
	PowerState  string    `json:"power_state"` // the VM's state when taken (poweredOn = includes memory)
	Quiesced    bool      `json:"quiesced"`
}

// NetOption / DSOption are choices offered when editing.
type NetOption struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
	Type string `json:"type"` // Network | DistributedVirtualPortgroup | OpaqueNetwork
}

type DSOption struct {
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	Free     int64  `json:"free"`
	Capacity int64  `json:"capacity"`
}

// VMConfig is everything the edit screen needs.
type VMConfig struct {
	Ref            string       `json:"ref"`
	Name           string       `json:"name"`
	Annotation     string       `json:"annotation"`
	PowerState     string       `json:"power_state"`
	Template       bool         `json:"template"`
	GuestOS        string       `json:"guest_os"`
	ToolsRunning   string       `json:"tools_running"`
	HostRef        string       `json:"host_ref"`
	CPUs           int32        `json:"cpus"`
	CoresPerSocket int32        `json:"cores_per_socket"`
	MemMB          int32        `json:"mem_mb"`
	CPUHotAdd      bool         `json:"cpu_hot_add"`
	CPUHotRemove   bool         `json:"cpu_hot_remove"`
	MemHotAdd      bool         `json:"mem_hot_add"`
	HWVersion      string       `json:"hw_version"`
	MaxCPUs        int32        `json:"max_cpus"`   // host's logical CPUs
	MaxMemMB       int64        `json:"max_mem_mb"` // host's memory
	Disks          []VMDisk     `json:"disks"`
	Nics           []VMNic      `json:"nics"`
	Cdroms         []VMCdrom    `json:"cdroms"`
	Snapshots      []VMSnapshot `json:"snapshots"`
	Networks       []NetOption  `json:"networks"`
	Datastores     []DSOption   `json:"datastores"`
}

func vmObj(c *Conn, ref string) *object.VirtualMachine {
	return object.NewVirtualMachine(c.c.Client, types.ManagedObjectReference{Type: "VirtualMachine", Value: ref})
}

// VMConfig reads a VM's editable configuration.
func (c *Conn) VMConfig(ctx context.Context, ref string) (*VMConfig, error) {
	vm := vmObj(c, ref)
	var m mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"name", "config", "runtime", "summary.guest", "snapshot", "network", "datastore"}, &m); err != nil {
		return nil, vmErr(err)
	}
	if m.Config == nil {
		return nil, errors.New("the VM's configuration is not available (is it inaccessible or orphaned?)")
	}
	cfg := m.Config
	out := &VMConfig{
		Ref: ref, Name: m.Name, Annotation: cfg.Annotation, PowerState: string(m.Runtime.PowerState), Template: cfg.Template,
		GuestOS: cfg.GuestFullName, CPUs: cfg.Hardware.NumCPU, CoresPerSocket: cfg.Hardware.NumCoresPerSocket, MemMB: cfg.Hardware.MemoryMB,
		HWVersion: cfg.Version,
	}
	if m.Summary.Guest != nil {
		out.ToolsRunning = m.Summary.Guest.ToolsRunningStatus
	}
	if out.CoresPerSocket <= 0 {
		out.CoresPerSocket = 1
	}
	out.CPUHotAdd = cfg.CpuHotAddEnabled != nil && *cfg.CpuHotAddEnabled
	out.CPUHotRemove = cfg.CpuHotRemoveEnabled != nil && *cfg.CpuHotRemoveEnabled
	out.MemHotAdd = cfg.MemoryHotAddEnabled != nil && *cfg.MemoryHotAddEnabled

	pc := property.DefaultCollector(c.c.Client)
	names := map[string]string{} // datastore / network ref -> name
	if len(m.Datastore) > 0 {
		var dss []mo.Datastore
		_ = pc.Retrieve(ctx, m.Datastore, []string{"name"}, &dss)
		for _, d := range dss {
			names[d.Self.Value] = d.Name
		}
	}

	// the host: its CPU / memory limits, networks and datastores
	var hostNets, hostDS []types.ManagedObjectReference
	if m.Runtime.Host != nil {
		out.HostRef = m.Runtime.Host.Value
		var h mo.HostSystem
		if err := pc.RetrieveOne(ctx, *m.Runtime.Host, []string{"summary.hardware", "network", "datastore"}, &h); err == nil {
			if h.Summary.Hardware != nil {
				out.MaxCPUs = int32(h.Summary.Hardware.NumCpuThreads)
				out.MaxMemMB = h.Summary.Hardware.MemorySize / (1 << 20)
			}
			hostNets, hostDS = h.Network, h.Datastore
		}
	}
	if len(hostNets) == 0 {
		hostNets = m.Network
	}
	if len(hostNets) > 0 {
		var nets []mo.Network
		_ = pc.Retrieve(ctx, hostNets, []string{"name"}, &nets)
		for _, n := range nets {
			names[n.Self.Value] = n.Name
			if strings.HasSuffix(n.Name, "-DVUplinks") || strings.Contains(strings.ToLower(n.Name), "uplinks-") {
				continue // uplink port groups aren't for VMs
			}
			out.Networks = append(out.Networks, NetOption{Ref: n.Self.Value, Name: n.Name, Type: n.Self.Type})
		}
		sort.Slice(out.Networks, func(i, j int) bool { return out.Networks[i].Name < out.Networks[j].Name })
	}
	if len(hostDS) == 0 {
		hostDS = m.Datastore
	}
	if len(hostDS) > 0 {
		var dss []mo.Datastore
		_ = pc.Retrieve(ctx, hostDS, []string{"name", "summary"}, &dss)
		for _, d := range dss {
			names[d.Self.Value] = d.Name
			if !d.Summary.Accessible {
				continue
			}
			out.Datastores = append(out.Datastores, DSOption{Ref: d.Self.Value, Name: d.Name, Free: d.Summary.FreeSpace, Capacity: d.Summary.Capacity})
		}
		sort.Slice(out.Datastores, func(i, j int) bool { return out.Datastores[i].Name < out.Datastores[j].Name })
	}
	// distributed port groups the adapters use (by key)
	dvpg := map[string]string{}
	for _, n := range out.Networks {
		if n.Type == "DistributedVirtualPortgroup" {
			dvpg[n.Ref] = n.Name
		}
	}

	devs := object.VirtualDeviceList(cfg.Hardware.Device)
	for _, d := range devs {
		label := devs.Name(d)
		if di := d.GetVirtualDevice().DeviceInfo; di != nil && di.GetDescription() != nil && di.GetDescription().Label != "" {
			label = di.GetDescription().Label
		}
		switch x := d.(type) {
		case *types.VirtualDisk:
			disk := VMDisk{Key: x.Key, Label: label, SizeBytes: x.CapacityInBytes, Unit: diskUnit(devs, x)}
			if disk.SizeBytes == 0 {
				disk.SizeBytes = x.CapacityInKB * 1024
			}
			if b, ok := x.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				disk.File = b.FileName
				disk.Thin = b.ThinProvisioned != nil && *b.ThinProvisioned
				if b.Datastore != nil {
					disk.Datastore = names[b.Datastore.Value]
				}
			} else if b, ok := x.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				disk.File = b.GetVirtualDeviceFileBackingInfo().FileName
			}
			if disk.Datastore == "" {
				disk.Datastore = dsOfPath(disk.File)
			}
			out.Disks = append(out.Disks, disk)
		case types.BaseVirtualEthernetCard:
			card := x.GetVirtualEthernetCard()
			nic := VMNic{Key: card.Key, Label: label, Type: strings.TrimPrefix(devs.Type(d), "virtual"), MAC: card.MacAddress}
			if cn := card.Connectable; cn != nil {
				nic.Connected, nic.OnBoot = cn.Connected, cn.StartConnected
			}
			switch b := card.Backing.(type) {
			case *types.VirtualEthernetCardNetworkBackingInfo:
				nic.Network = b.DeviceName
				if b.Network != nil {
					nic.NetRef = b.Network.Value
				}
			case *types.VirtualEthernetCardDistributedVirtualPortBackingInfo:
				nic.NetRef = b.Port.PortgroupKey
				nic.Network = dvpg[b.Port.PortgroupKey]
				if nic.Network == "" {
					nic.Network = b.Port.PortgroupKey
				}
			case *types.VirtualEthernetCardOpaqueNetworkBackingInfo:
				nic.Network = b.OpaqueNetworkId
			}
			out.Nics = append(out.Nics, nic)
		case *types.VirtualCdrom:
			cd := VMCdrom{Key: x.Key, Label: label}
			if cn := x.Connectable; cn != nil {
				cd.Connected, cd.OnBoot = cn.Connected, cn.StartConnected
			}
			if b, ok := x.Backing.(*types.VirtualCdromIsoBackingInfo); ok {
				cd.ISO = b.FileName
			}
			out.Cdroms = append(out.Cdroms, cd)
		}
	}
	sort.Slice(out.Disks, func(i, j int) bool { return out.Disks[i].Key < out.Disks[j].Key })

	if m.Snapshot != nil {
		cur := ""
		if m.Snapshot.CurrentSnapshot != nil {
			cur = m.Snapshot.CurrentSnapshot.Value
		}
		var walk func(list []types.VirtualMachineSnapshotTree, parent string, depth int)
		walk = func(list []types.VirtualMachineSnapshotTree, parent string, depth int) {
			for _, s := range list {
				out.Snapshots = append(out.Snapshots, VMSnapshot{Ref: s.Snapshot.Value, Name: s.Name, Description: s.Description, Created: s.CreateTime,
					Parent: parent, Depth: depth, Current: s.Snapshot.Value == cur, PowerState: string(s.State), Quiesced: s.Quiesced})
				walk(s.ChildSnapshotList, s.Snapshot.Value, depth+1)
			}
		}
		walk(m.Snapshot.RootSnapshotList, "", 0)
	}
	if out.Disks == nil {
		out.Disks = []VMDisk{}
	}
	if out.Nics == nil {
		out.Nics = []VMNic{}
	}
	if out.Cdroms == nil {
		out.Cdroms = []VMCdrom{}
	}
	if out.Snapshots == nil {
		out.Snapshots = []VMSnapshot{}
	}
	return out, nil
}

func diskUnit(devs object.VirtualDeviceList, d *types.VirtualDisk) string {
	ctl := devs.FindByKey(d.ControllerKey)
	if ctl == nil || d.UnitNumber == nil {
		return ""
	}
	kind := "Disk"
	switch ctl.(type) {
	case types.BaseVirtualSCSIController:
		kind = "SCSI"
	case *types.VirtualNVMEController:
		kind = "NVMe"
	case *types.VirtualAHCIController:
		kind = "SATA"
	case *types.VirtualIDEController:
		kind = "IDE"
	}
	bus := int32(0)
	if c, ok := ctl.(types.BaseVirtualController); ok {
		bus = c.GetVirtualController().BusNumber
	}
	return fmt.Sprintf("%s %d:%d", kind, bus, *d.UnitNumber)
}

func dsOfPath(p string) string {
	if strings.HasPrefix(p, "[") {
		if i := strings.Index(p, "]"); i > 0 {
			return p[1:i]
		}
	}
	return ""
}

// ---- changes ----

// VMSettings changes general settings; nil fields are left alone.
type VMSettings struct {
	Name           *string `json:"name"`
	Annotation     *string `json:"annotation"`
	CPUs           *int32  `json:"cpus"`
	CoresPerSocket *int32  `json:"cores_per_socket"`
	MemMB          *int32  `json:"mem_mb"`
}

// Validate checks a settings change against the VM's current state. It returns a message an
// operator can act on (power the VM off, enable hot-add…).
func (s VMSettings) Validate(cur *VMConfig) error {
	on := cur.PowerState == "poweredOn"
	if s.Name != nil {
		n := strings.TrimSpace(*s.Name)
		if n == "" || len(n) > 80 {
			return errors.New("the VM name must be 1–80 characters")
		}
		if strings.ContainsAny(n, `%/\`) {
			return errors.New(`the VM name can't contain %, / or \`)
		}
	}
	if s.Annotation != nil && len(*s.Annotation) > 8000 {
		return errors.New("the notes are too long")
	}
	cpus, cps := cur.CPUs, cur.CoresPerSocket
	if s.CPUs != nil {
		cpus = *s.CPUs
	}
	if s.CoresPerSocket != nil {
		cps = *s.CoresPerSocket
	}
	if s.CPUs != nil || s.CoresPerSocket != nil {
		if cpus < 1 || cpus > 768 {
			return errors.New("vCPUs must be between 1 and 768")
		}
		if cur.MaxCPUs > 0 && cpus > cur.MaxCPUs {
			return fmt.Errorf("the host has %d logical CPUs; a VM can't have more vCPUs than that", cur.MaxCPUs)
		}
		if cps < 1 || cpus%cps != 0 {
			return fmt.Errorf("%d vCPUs can't be split into sockets of %d cores", cpus, cps)
		}
		if on {
			if cps != cur.CoresPerSocket {
				return errors.New("cores per socket can only be changed while the VM is powered off")
			}
			if cpus > cur.CPUs && !cur.CPUHotAdd {
				return errors.New("CPU hot add is not enabled for this VM: power it off to add vCPUs")
			}
			if cpus < cur.CPUs && !cur.CPUHotRemove {
				return errors.New("vCPUs can only be removed while the VM is powered off")
			}
		}
	}
	if s.MemMB != nil {
		mem := *s.MemMB
		if mem < 4 || mem%4 != 0 {
			return errors.New("memory must be a multiple of 4 MB")
		}
		if cur.MaxMemMB > 0 && int64(mem) > cur.MaxMemMB {
			return fmt.Errorf("the host has %d GB of memory; give the VM less", cur.MaxMemMB/1024)
		}
		if on && mem < cur.MemMB {
			return errors.New("memory can only be reduced while the VM is powered off")
		}
		if on && mem > cur.MemMB && !cur.MemHotAdd {
			return errors.New("memory hot add is not enabled for this VM: power it off to add memory")
		}
	}
	return nil
}

// ConfigureVM applies general settings in one reconfigure task.
func (c *Conn) ConfigureVM(ctx context.Context, ref string, s VMSettings) error {
	spec := types.VirtualMachineConfigSpec{}
	if s.Name != nil {
		spec.Name = strings.TrimSpace(*s.Name)
	}
	if s.Annotation != nil {
		spec.Annotation = *s.Annotation
		if spec.Annotation == "" {
			spec.Annotation = " " // an empty string means "leave unchanged" to vSphere
		}
	}
	if s.CPUs != nil {
		spec.NumCPUs = *s.CPUs
	}
	if s.CoresPerSocket != nil {
		spec.NumCoresPerSocket = *s.CoresPerSocket
	}
	if s.MemMB != nil {
		spec.MemoryMB = int64(*s.MemMB)
	}
	return c.reconfig(ctx, ref, spec)
}

func (c *Conn) reconfig(ctx context.Context, ref string, spec types.VirtualMachineConfigSpec) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	t, err := vmObj(c, ref).Reconfigure(ctx, spec)
	if err == nil {
		err = t.Wait(ctx)
	}
	return vmErr(err)
}

func (c *Conn) devices(ctx context.Context, ref string) (object.VirtualDeviceList, error) {
	l, err := vmObj(c, ref).Device(ctx)
	return l, vmErr(err)
}

func findKey[T types.BaseVirtualDevice](l object.VirtualDeviceList, key int32, what string) (T, error) {
	var zero T
	d := l.FindByKey(key)
	if d == nil {
		return zero, fmt.Errorf("that %s is no longer on the VM; refresh and try again", what)
	}
	x, ok := d.(T)
	if !ok {
		return zero, fmt.Errorf("device %d is not a %s", key, what)
	}
	return x, nil
}

// ResizeDisk grows a disk. vSphere can't shrink disks.
func (c *Conn) ResizeDisk(ctx context.Context, ref string, key int32, newBytes int64) error {
	l, err := c.devices(ctx, ref)
	if err != nil {
		return err
	}
	d, err := findKey[*types.VirtualDisk](l, key, "disk")
	if err != nil {
		return err
	}
	cur := d.CapacityInBytes
	if cur == 0 {
		cur = d.CapacityInKB * 1024
	}
	if newBytes <= cur {
		return errors.New("disks can only be made larger")
	}
	d.CapacityInBytes = newBytes
	d.CapacityInKB = newBytes / 1024
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: d},
	}}
	return c.reconfig(ctx, ref, spec)
}

// AddDisk creates a new disk on a datastore (thin or thick provisioned).
func (c *Conn) AddDisk(ctx context.Context, ref string, bytes int64, dsRef string, thin bool) error {
	l, err := c.devices(ctx, ref)
	if err != nil {
		return err
	}
	ctl, err := l.FindDiskController("")
	if err != nil {
		return errors.New("the VM has no disk controller with a free slot")
	}
	ds := types.ManagedObjectReference{Type: "Datastore", Value: dsRef}
	var dsName string
	var dm mo.Datastore
	if err := property.DefaultCollector(c.c.Client).RetrieveOne(ctx, ds, []string{"name"}, &dm); err != nil {
		return errors.New("that datastore isn't available")
	}
	dsName = dm.Name
	disk := l.CreateDisk(ctl, ds, "")
	if b, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
		// "[datastore]" alone lets vSphere name the file in the VM's folder on that datastore
		b.FileName = "[" + dsName + "]"
		b.ThinProvisioned = types.NewBool(thin)
		if !thin {
			b.EagerlyScrub = types.NewBool(false) // lazy-zeroed thick
		}
	}
	disk.CapacityInBytes = bytes
	disk.CapacityInKB = bytes / 1024
	if disk.UnitNumber == nil || *disk.UnitNumber < 0 {
		return errors.New("the VM's disk controller has no free slot")
	}
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, FileOperation: types.VirtualDeviceConfigSpecFileOperationCreate, Device: disk},
	}}
	return c.reconfig(ctx, ref, spec)
}

// RemoveDisk detaches a disk, deleting its file when deleteFiles is set.
func (c *Conn) RemoveDisk(ctx context.Context, ref string, key int32, deleteFiles bool) error {
	l, err := c.devices(ctx, ref)
	if err != nil {
		return err
	}
	d, err := findKey[*types.VirtualDisk](l, key, "disk")
	if err != nil {
		return err
	}
	ch := &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationRemove, Device: d}
	if deleteFiles {
		ch.FileOperation = types.VirtualDeviceConfigSpecFileOperationDestroy
	}
	return c.reconfig(ctx, ref, types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{ch}})
}

// SetNic moves an adapter to a network (netRef empty: keep) and connects or disconnects it.
func (c *Conn) SetNic(ctx context.Context, ref string, key int32, netRef string, connected *bool) error {
	l, err := c.devices(ctx, ref)
	if err != nil {
		return err
	}
	d := l.FindByKey(key)
	card, ok := d.(types.BaseVirtualEthernetCard)
	if d == nil || !ok {
		return errors.New("that network adapter is no longer on the VM; refresh and try again")
	}
	if netRef != "" {
		typ := "Network"
		if strings.HasPrefix(netRef, "dvportgroup-") {
			typ = "DistributedVirtualPortgroup"
		}
		o := object.NewReference(c.c.Client, types.ManagedObjectReference{Type: typ, Value: netRef})
		nr, ok := o.(object.NetworkReference)
		if !ok {
			return errors.New("that network can't be used for an adapter")
		}
		backing, err := nr.EthernetCardBackingInfo(ctx)
		if err != nil {
			return vmErr(err)
		}
		card.GetVirtualEthernetCard().Backing = backing
	}
	if connected != nil {
		e := card.GetVirtualEthernetCard()
		if e.Connectable == nil {
			e.Connectable = &types.VirtualDeviceConnectInfo{AllowGuestControl: true}
		}
		e.Connectable.Connected, e.Connectable.StartConnected = *connected, *connected
	}
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: d},
	}}
	return c.reconfig(ctx, ref, spec)
}

// SetCdrom mounts an ISO ("[datastore] path.iso"), or ejects when iso is empty.
func (c *Conn) SetCdrom(ctx context.Context, ref string, key int32, iso string, connected *bool) error {
	l, err := c.devices(ctx, ref)
	if err != nil {
		return err
	}
	cd, err := findKey[*types.VirtualCdrom](l, key, "CD/DVD drive")
	if err != nil {
		return err
	}
	if iso != "" {
		l.InsertIso(cd, iso)
	} else {
		l.EjectIso(cd)
	}
	if cd.Connectable == nil {
		cd.Connectable = &types.VirtualDeviceConnectInfo{AllowGuestControl: true}
	}
	conn := iso != ""
	if connected != nil {
		conn = *connected
	}
	cd.Connectable.Connected, cd.Connectable.StartConnected = conn, conn
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: cd},
	}}
	return c.reconfig(ctx, ref, spec)
}

// ISOFile is an .iso found on a datastore.
type ISOFile struct {
	Path string `json:"path"` // "[datastore] folder/file.iso"
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ListISOs searches a datastore for .iso files (all folders).
func (c *Conn) ListISOs(ctx context.Context, dsRef string) ([]ISOFile, error) {
	ds := object.NewDatastore(c.c.Client, types.ManagedObjectReference{Type: "Datastore", Value: dsRef})
	var dm mo.Datastore
	if err := ds.Properties(ctx, ds.Reference(), []string{"name", "browser"}, &dm); err != nil {
		return nil, vmErr(err)
	}
	ds.InventoryPath = dm.Name
	b := object.NewHostDatastoreBrowser(c.c.Client, dm.Browser)
	spec := types.HostDatastoreBrowserSearchSpec{
		MatchPattern: []string{"*.iso", "*.ISO"},
		Details:      &types.FileQueryFlags{FileSize: true, FileType: true, Modification: true},
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	t, err := b.SearchDatastoreSubFolders(ctx, "["+dm.Name+"]", &spec)
	if err != nil {
		return nil, vmErr(err)
	}
	info, err := t.WaitForResult(ctx, nil)
	if err != nil {
		return nil, vmErr(err)
	}
	var out []ISOFile
	if res, ok := info.Result.(types.ArrayOfHostDatastoreBrowserSearchResults); ok {
		for _, r := range res.HostDatastoreBrowserSearchResults {
			for _, f := range r.File {
				fi := f.GetFileInfo()
				p := isoPath(r.FolderPath, fi.Path)
				out = append(out, ISOFile{Path: p, Name: path.Base(fi.Path), Size: fi.FileSize})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path) })
	if len(out) > 1000 {
		out = out[:1000]
	}
	return out, nil
}

// isoPath joins a search result folder ("[ds] dir/", "[ds]" or "[ds]/dir") and a file name into
// the datastore path form vSphere expects: "[ds] dir/file.iso".
func isoPath(folder, file string) string {
	ds, rest := "", folder
	if strings.HasPrefix(folder, "[") {
		if i := strings.Index(folder, "]"); i > 0 {
			ds, rest = folder[:i+1], folder[i+1:]
		}
	}
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	if rest != "" {
		rest += "/"
	}
	return strings.TrimSpace(ds + " " + rest + file)
}

// ---- snapshots ----

// TakeSnapshot creates a snapshot; memory includes the running state, quiesce asks VMware
// Tools to flush the guest's file systems first.
func (c *Conn) TakeSnapshot(ctx context.Context, ref, name, desc string, memory, quiesce bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	t, err := vmObj(c, ref).CreateSnapshot(ctx, name, desc, memory, quiesce)
	if err == nil {
		err = t.Wait(ctx)
	}
	return vmErr(err)
}

// RevertSnapshot goes back to a snapshot. A snapshot taken with memory resumes running.
func (c *Conn) RevertSnapshot(ctx context.Context, snapRef string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	no := false
	res, err := methods.RevertToSnapshot_Task(ctx, c.c.Client, &types.RevertToSnapshot_Task{
		This: types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: snapRef}, SuppressPowerOn: &no})
	if err == nil {
		err = object.NewTask(c.c.Client, res.Returnval).Wait(ctx)
	}
	return vmErr(err)
}

// DeleteSnapshot removes a snapshot (and its children when asked), consolidating disks.
func (c *Conn) DeleteSnapshot(ctx context.Context, snapRef string, children bool) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	yes := true
	res, err := methods.RemoveSnapshot_Task(ctx, c.c.Client, &types.RemoveSnapshot_Task{
		This: types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: snapRef}, RemoveChildren: children, Consolidate: &yes})
	if err == nil {
		err = object.NewTask(c.c.Client, res.Returnval).Wait(ctx)
	}
	return vmErr(err)
}

// vmErr turns vSphere faults into short messages.
func vmErr(err error) error {
	if err == nil {
		return nil
	}
	var fault types.BaseMethodFault
	var te task.Error
	msg := ""
	switch {
	case errors.As(err, &te):
		fault = te.Fault()
		if lm := te.LocalizedMethodFault; lm != nil {
			msg = lm.LocalizedMessage
		}
	case soap.IsVimFault(err):
		fault = soap.ToVimFault(err)
	}
	switch f := fault.(type) {
	case *types.InvalidPowerState:
		return fmt.Errorf("the VM is %s, so that change isn't possible now", stateWord(string(f.ExistingState)))
	case *types.NoPermission:
		return errors.New("the vCenter account does not have permission for that change (it needs the Virtual machine › Change configuration / Snapshot management privileges)")
	case *types.InsufficientResourcesFault:
		return errors.New("the host doesn't have enough resources for that")
	case *types.FileAlreadyExists:
		return errors.New("a file with that name already exists on the datastore")
	case *types.NoDiskSpace:
		return errors.New("the datastore doesn't have enough free space")
	case *types.ToolsUnavailable:
		return errors.New("VMware Tools is not running, so the guest can't be quiesced; take the snapshot without quiescing")
	case *types.ManagedObjectNotFound:
		return errors.New("that object no longer exists in vSphere; refresh and try again")
	}
	if msg != "" {
		return errors.New(msg)
	}
	return powerErr(err)
}
