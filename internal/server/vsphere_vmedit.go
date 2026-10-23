package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/vsphere"
)

// Editing VMs: CPU / memory, name and notes, disks, network adapters, CD/DVD and snapshots.
// Reading the configuration is open to every role; changes are admin-only and audited.

const maxDiskBytes = 62 << 40 // vSphere's largest virtual disk (62 TB)

// vmTarget resolves {id}/{ref} to a signed-in session and the VM from the last inventory.
func (s *Server) vmTarget(w http.ResponseWriter, r *http.Request) (*vsConn, *vsphere.Conn, *vsphere.VM, bool) {
	ctx := r.Context()
	id, ref := chi.URLParam(r, "id"), chi.URLParam(r, "ref")
	if !refRe.MatchString(ref) {
		fail(w, badRequest("invalid VM reference"))
		return nil, nil, nil, false
	}
	c, err := s.vsLoad(ctx, id)
	if err != nil {
		fail(w, err)
		return nil, nil, nil, false
	}
	inv, _, err := s.loadInventory(ctx, id)
	if err != nil {
		fail(w, err)
		return nil, nil, nil, false
	}
	var vm *vsphere.VM
	if inv != nil {
		for i := range inv.VMs {
			if inv.VMs[i].Ref == ref {
				vm = &inv.VMs[i]
			}
		}
	}
	if vm == nil {
		fail(w, ErrNotFound)
		return nil, nil, nil, false
	}
	conn, err := s.vs.session(ctx, c)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return nil, nil, nil, false
	}
	return c, conn, vm, true
}

// afterVMChange records the change and refreshes the inventory shortly after.
func (s *Server) afterVMChange(r *http.Request, c *vsConn, vm *vsphere.VM, what string, details map[string]any, err error) {
	if details == nil {
		details = map[string]any{}
	}
	details["connection"], details["vm"], details["change"], details["ok"] = c.Name, vm.Name, what, err == nil
	if err != nil {
		details["error"] = err.Error()
	}
	s.audit(r.Context(), userFrom(r), r, "vsphere.vm_edit", "vsphere_vm", vm.Ref, details)
	if err == nil {
		id := c.ID
		go func() {
			time.Sleep(2 * time.Second)
			_ = s.vs.poll(context.Background(), id)
		}()
	}
}

// GET /api/vsphere/connections/{id}/vms/{ref}/config
func (s *Server) vsphereVMConfig(w http.ResponseWriter, r *http.Request) {
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	cfg, err := conn.VMConfig(r.Context(), vm.Ref)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"connection": map[string]any{"id": c.ID, "name": c.Name}, "config": cfg})
}

// PATCH /api/vsphere/connections/{id}/vms/{ref}/config {name?, annotation?, cpus?, cores_per_socket?, mem_mb?}
func (s *Server) vsphereVMSettings(w http.ResponseWriter, r *http.Request) {
	var in vsphere.VMSettings
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	if vm.Template {
		fail(w, badRequest("templates can't be edited here; convert it to a VM in vSphere first"))
		return
	}
	cur, err := conn.VMConfig(r.Context(), vm.Ref)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	// leave out what didn't change
	if in.Name != nil && strings.TrimSpace(*in.Name) == cur.Name {
		in.Name = nil
	}
	if in.Annotation != nil && *in.Annotation == cur.Annotation {
		in.Annotation = nil
	}
	if in.CPUs != nil && *in.CPUs == cur.CPUs {
		in.CPUs = nil
	}
	if in.CoresPerSocket != nil && *in.CoresPerSocket == cur.CoresPerSocket {
		in.CoresPerSocket = nil
	}
	if in.MemMB != nil && *in.MemMB == cur.MemMB {
		in.MemMB = nil
	}
	if in == (vsphere.VMSettings{}) {
		writeJSON(w, 200, map[string]any{"ok": true, "unchanged": true})
		return
	}
	if err := in.Validate(cur); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	err = conn.ConfigureVM(r.Context(), vm.Ref, in)
	d := map[string]any{}
	if in.Name != nil {
		d["name"] = map[string]any{"from": cur.Name, "to": *in.Name}
	}
	if in.Annotation != nil {
		d["notes"] = true
	}
	if in.CPUs != nil {
		d["cpus"] = map[string]any{"from": cur.CPUs, "to": *in.CPUs}
	}
	if in.CoresPerSocket != nil {
		d["cores_per_socket"] = map[string]any{"from": cur.CoresPerSocket, "to": *in.CoresPerSocket}
	}
	if in.MemMB != nil {
		d["mem_mb"] = map[string]any{"from": cur.MemMB, "to": *in.MemMB}
	}
	s.afterVMChange(r, c, vm, "settings", d, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func keyParam(r *http.Request, name string) (int32, error) {
	k, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil {
		return 0, badRequest("invalid device key")
	}
	return int32(k), nil
}

func gbToBytes(gb float64) (int64, error) {
	if gb <= 0 || gb*(1<<30) > maxDiskBytes {
		return 0, badRequest("disk size must be between 1 MB and 62 TB")
	}
	b := int64(gb * (1 << 30))
	return b - b%(1<<20), nil // whole MB
}

// POST /api/vsphere/connections/{id}/vms/{ref}/disks {gb, datastore, thin}
func (s *Server) vsphereAddDisk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GB        float64 `json:"gb"`
		Datastore string  `json:"datastore"`
		Thin      *bool   `json:"thin"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	bytes, err := gbToBytes(in.GB)
	if err != nil {
		fail(w, err)
		return
	}
	if !refRe.MatchString(in.Datastore) {
		fail(w, badRequest("choose a datastore"))
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	thin := in.Thin == nil || *in.Thin
	err = conn.AddDisk(r.Context(), vm.Ref, bytes, in.Datastore, thin)
	s.afterVMChange(r, c, vm, "add_disk", map[string]any{"gb": in.GB, "datastore": in.Datastore, "thin": thin}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// PATCH /api/vsphere/connections/{id}/vms/{ref}/disks/{key} {gb}
func (s *Server) vsphereResizeDisk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GB float64 `json:"gb"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	key, err := keyParam(r, "key")
	if err != nil {
		fail(w, err)
		return
	}
	bytes, err := gbToBytes(in.GB)
	if err != nil {
		fail(w, err)
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	err = conn.ResizeDisk(r.Context(), vm.Ref, key, bytes)
	s.afterVMChange(r, c, vm, "grow_disk", map[string]any{"disk": key, "gb": in.GB}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// DELETE /api/vsphere/connections/{id}/vms/{ref}/disks/{key}?delete_files=1
func (s *Server) vsphereRemoveDisk(w http.ResponseWriter, r *http.Request) {
	key, err := keyParam(r, "key")
	if err != nil {
		fail(w, err)
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	del := r.URL.Query().Get("delete_files") == "1"
	err = conn.RemoveDisk(r.Context(), vm.Ref, key, del)
	s.afterVMChange(r, c, vm, "remove_disk", map[string]any{"disk": key, "delete_files": del}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// PATCH /api/vsphere/connections/{id}/vms/{ref}/nics/{key} {network?, connected?}
func (s *Server) vsphereSetNic(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Network   string `json:"network"`
		Connected *bool  `json:"connected"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	key, err := keyParam(r, "key")
	if err != nil {
		fail(w, err)
		return
	}
	if in.Network != "" && !refRe.MatchString(in.Network) {
		fail(w, badRequest("invalid network"))
		return
	}
	if in.Network == "" && in.Connected == nil {
		fail(w, badRequest("nothing to change"))
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	err = conn.SetNic(r.Context(), vm.Ref, key, in.Network, in.Connected)
	d := map[string]any{"nic": key}
	if in.Network != "" {
		d["network"] = in.Network
	}
	if in.Connected != nil {
		d["connected"] = *in.Connected
	}
	s.afterVMChange(r, c, vm, "nic", d, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// PATCH /api/vsphere/connections/{id}/vms/{ref}/cdroms/{key} {iso, connected?} — iso "" ejects.
func (s *Server) vsphereSetCdrom(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ISO       string `json:"iso"`
		Connected *bool  `json:"connected"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	key, err := keyParam(r, "key")
	if err != nil {
		fail(w, err)
		return
	}
	in.ISO = strings.TrimSpace(in.ISO)
	if in.ISO != "" && (!strings.HasPrefix(in.ISO, "[") || !strings.HasSuffix(strings.ToLower(in.ISO), ".iso") || strings.Contains(in.ISO, "..")) {
		fail(w, badRequest(`pick an .iso file on a datastore ("[datastore] folder/file.iso")`))
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	err = conn.SetCdrom(r.Context(), vm.Ref, key, in.ISO, in.Connected)
	what := "mount_iso"
	if in.ISO == "" {
		what = "eject_iso"
	}
	s.afterVMChange(r, c, vm, what, map[string]any{"cdrom": key, "iso": in.ISO}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/vsphere/connections/{id}/datastores/{ds}/isos
func (s *Server) vsphereISOs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ds := chi.URLParam(r, "ds")
	if !refRe.MatchString(ds) {
		fail(w, badRequest("invalid datastore"))
		return
	}
	c, err := s.vsLoad(ctx, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return
	}
	conn, err := s.vs.session(ctx, c)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	isos, err := conn.ListISOs(ctx, ds)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if isos == nil {
		isos = []vsphere.ISOFile{}
	}
	writeJSON(w, 200, isos)
}

// vmSnapshot checks that a snapshot belongs to the VM and returns its name.
func vmSnapshot(conn *vsphere.Conn, r *http.Request, vmRef, snap string) (string, error) {
	if !refRe.MatchString(snap) {
		return "", badRequest("invalid snapshot")
	}
	cfg, err := conn.VMConfig(r.Context(), vmRef)
	if err != nil {
		return "", badRequest(err.Error())
	}
	for _, s := range cfg.Snapshots {
		if s.Ref == snap {
			return s.Name, nil
		}
	}
	return "", ErrNotFound
}

// POST /api/vsphere/connections/{id}/vms/{ref}/snapshots {name, description, memory, quiesce}
func (s *Server) vsphereTakeSnapshot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Memory      bool   `json:"memory"`
		Quiesce     bool   `json:"quiesce"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 80 {
		fail(w, badRequest("give the snapshot a name (up to 80 characters)"))
		return
	}
	if len(in.Description) > 2000 {
		fail(w, badRequest("the description is too long"))
		return
	}
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	if vm.PowerState != "poweredOn" {
		in.Memory, in.Quiesce = false, false
	}
	if in.Memory {
		in.Quiesce = false // vSphere quiesces only snapshots without memory
	}
	err := conn.TakeSnapshot(r.Context(), vm.Ref, in.Name, in.Description, in.Memory, in.Quiesce)
	s.afterVMChange(r, c, vm, "snapshot_take", map[string]any{"snapshot": in.Name, "memory": in.Memory, "quiesce": in.Quiesce}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/vsphere/connections/{id}/vms/{ref}/snapshots/{snap}/revert
func (s *Server) vsphereRevertSnapshot(w http.ResponseWriter, r *http.Request) {
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	snap := chi.URLParam(r, "snap")
	name, err := vmSnapshot(conn, r, vm.Ref, snap)
	if err != nil {
		fail(w, err)
		return
	}
	err = conn.RevertSnapshot(r.Context(), snap)
	s.afterVMChange(r, c, vm, "snapshot_revert", map[string]any{"snapshot": name}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// DELETE /api/vsphere/connections/{id}/vms/{ref}/snapshots/{snap}?children=1
func (s *Server) vsphereDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	c, conn, vm, ok := s.vmTarget(w, r)
	if !ok {
		return
	}
	snap := chi.URLParam(r, "snap")
	name, err := vmSnapshot(conn, r, vm.Ref, snap)
	if err != nil {
		fail(w, err)
		return
	}
	children := r.URL.Query().Get("children") == "1"
	err = conn.DeleteSnapshot(r.Context(), snap, children)
	s.afterVMChange(r, c, vm, "snapshot_delete", map[string]any{"snapshot": name, "children": children}, err)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- the VM behind an agent device ----

// GET /api/devices/{id}/vm — the vSphere VM this device runs in, if Apex knows it.
func (s *Server) deviceVM(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	conns, err := s.db.Maps(ctx, `SELECT id, name, url FROM vsphere_connections WHERE enabled ORDER BY created_at`)
	if err != nil {
		fail(w, err)
		return
	}
	for _, c := range conns {
		cid := c["id"].(string)
		inv, _, err := s.loadInventory(ctx, cid)
		if err != nil || inv == nil {
			continue
		}
		for ref, l := range s.vsAgentLinks(ctx, inv.VMs) {
			if l.DeviceID != id {
				continue
			}
			for _, vm := range inv.VMs {
				if vm.Ref != ref {
					continue
				}
				v := vmView{VM: vm, Agent: &l}
				if endpoint, err := vsphere.ParseURL(c["url"].(string)); err == nil {
					v.UIURL = vsphere.VMLink(endpoint, inv.About, vm.Ref)
				}
				writeJSON(w, 200, map[string]any{"connection": map[string]any{"id": cid, "name": c["name"]}, "vm": v})
				return
			}
		}
	}
	writeJSON(w, 200, map[string]any{"connection": nil, "vm": nil})
}
