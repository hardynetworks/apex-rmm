package server

import (
	"context"
	"testing"
)

// VM editing through the API against the vCenter simulator, and finding the VM behind a device.
func TestVSphereVMEditing(t *testing.T) {
	e := newITEnv(t)
	ctx := context.Background()
	var clientID string
	_ = e.s.db.QueryRow(ctx, `SELECT id FROM clients LIMIT 1`).Scan(&clientID)
	_, cm, _ := e.do("admin", "POST", "/api/vsphere/test", map[string]any{"url": e.sim, "username": "user", "password": "pass"})
	_, vm0, raw := e.do("admin", "POST", "/api/vsphere/connections", map[string]any{"name": "edit-lab", "url": e.sim, "username": "user", "password": "pass",
		"thumbprint": cm["thumbprint"], "enabled": true, "poll_seconds": 300, "ds_warn_pct": 85, "ds_crit_pct": 95, "snapshot_days": 3, "client_id": clientID})
	conn, _ := vm0["id"].(string)
	if conn == "" {
		t.Fatalf("connection: %s", raw)
	}
	e.pollNow(conn)
	inv, _, _ := e.s.loadInventory(ctx, conn)
	vm := inv.VMs[0]
	for _, v := range inv.VMs {
		if !v.Template {
			vm = v
			break
		}
	}
	base := "/api/vsphere/connections/" + conn + "/vms/" + vm.Ref

	// a device whose serial is the VM's BIOS UUID is that VM
	var dev string
	if err := e.s.db.QueryRow(ctx, `INSERT INTO devices (hostname, secret_hash, serial) VALUES ('vm-guest', 'x', $1) RETURNING id`, "VMware-"+vm.UUID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	c, m, raw := e.do("viewer", "GET", "/api/devices/"+dev+"/vm", nil)
	if c != 200 || m["vm"] == nil || m["vm"].(map[string]any)["ref"] != vm.Ref || m["connection"].(map[string]any)["id"] != conn {
		t.Fatalf("device vm: %d %s", c, raw)
	}
	var other string
	_ = e.s.db.QueryRow(ctx, `INSERT INTO devices (hostname, secret_hash) VALUES ('physical-box', 'x') RETURNING id`).Scan(&other)
	if _, m, _ := e.do("viewer", "GET", "/api/devices/"+other+"/vm", nil); m["vm"] != nil {
		t.Fatal("a physical device got a VM")
	}

	// read the configuration (any role)
	c, m, raw = e.do("viewer", "GET", base+"/config", nil)
	if c != 200 {
		t.Fatalf("config: %d %s", c, raw)
	}
	cfg := m["config"].(map[string]any)
	disks, nics := cfg["disks"].([]any), cfg["nics"].([]any)
	dss, nets := cfg["datastores"].([]any), cfg["networks"].([]any)
	if len(disks) == 0 || len(nics) == 0 || len(dss) == 0 || len(nets) == 0 {
		t.Fatalf("config: %s", raw)
	}

	// changes are admin-only
	if c, _, _ := e.do("technician", "PATCH", base+"/config", map[string]any{"cpus": 2}); c != 403 {
		t.Fatalf("technician edited a VM: %d", c)
	}
	// running VM without hot add: CPU change refused with a reason
	if c, m, _ := e.do("admin", "PATCH", base+"/config", map[string]any{"cpus": 2}); c != 400 || m["error"] == nil {
		t.Fatalf("hot add should be refused: %d %v", c, m)
	}
	if c, _, raw := e.do("technician", "POST", base+"/power", map[string]any{"action": "off"}); c != 200 {
		t.Fatalf("power off: %s", raw)
	}
	if c, _, raw := e.do("admin", "PATCH", base+"/config", map[string]any{"cpus": 2, "cores_per_socket": 2, "mem_mb": 2048, "name": "edited-vm", "annotation": "notes"}); c != 200 {
		t.Fatalf("settings: %s", raw)
	}
	_, m, _ = e.do("viewer", "GET", base+"/config", nil)
	cfg = m["config"].(map[string]any)
	if cfg["cpus"] != 2.0 || cfg["cores_per_socket"] != 2.0 || cfg["mem_mb"] != 2048.0 || cfg["name"] != "edited-vm" || cfg["annotation"] != "notes" {
		t.Fatalf("after settings: %v", cfg)
	}

	// disks
	d0 := disks[0].(map[string]any)
	key := itoa(int64(d0["key"].(float64)))
	gb := d0["size_bytes"].(float64)/(1<<30) + 1
	if c, _, raw := e.do("admin", "PATCH", base+"/disks/"+key, map[string]any{"gb": gb}); c != 200 {
		t.Fatalf("grow: %s", raw)
	}
	if c, _, _ := e.do("admin", "PATCH", base+"/disks/"+key, map[string]any{"gb": 0.5}); c != 400 {
		t.Fatal("shrink accepted")
	}
	ds := dss[0].(map[string]any)["ref"].(string)
	if c, _, raw := e.do("admin", "POST", base+"/disks", map[string]any{"gb": 3, "datastore": ds, "thin": true}); c != 200 {
		t.Fatalf("add disk: %s", raw)
	}
	_, m, _ = e.do("viewer", "GET", base+"/config", nil)
	disks = m["config"].(map[string]any)["disks"].([]any)
	if len(disks) != 2 {
		t.Fatalf("disks: %v", disks)
	}
	newKey := itoa(int64(disks[1].(map[string]any)["key"].(float64)))
	if c, _, raw := e.do("admin", "DELETE", base+"/disks/"+newKey+"?delete_files=1", nil); c != 200 {
		t.Fatalf("remove disk: %s", raw)
	}

	// network adapter
	nk := itoa(int64(nics[0].(map[string]any)["key"].(float64)))
	net := nets[len(nets)-1].(map[string]any)["ref"].(string)
	if c, _, raw := e.do("admin", "PATCH", base+"/nics/"+nk, map[string]any{"network": net, "connected": false}); c != 200 {
		t.Fatalf("nic: %s", raw)
	}

	// CD/DVD: bad paths refused, eject works
	if cds := m["config"].(map[string]any)["cdroms"].([]any); len(cds) > 0 {
		ck := itoa(int64(cds[0].(map[string]any)["key"].(float64)))
		if c, _, _ := e.do("admin", "PATCH", base+"/cdroms/"+ck, map[string]any{"iso": "/etc/passwd"}); c != 400 {
			t.Fatal("non-datastore path accepted")
		}
		if c, _, raw := e.do("admin", "PATCH", base+"/cdroms/"+ck, map[string]any{"iso": ""}); c != 200 {
			t.Fatalf("eject: %s", raw)
		}
	}
	if c, _, raw := e.do("admin", "GET", "/api/vsphere/connections/"+conn+"/datastores/"+ds+"/isos", nil); c != 200 {
		t.Fatalf("isos: %s", raw)
	}

	// snapshots
	if c, _, _ := e.do("admin", "POST", base+"/snapshots", map[string]any{"name": " "}); c != 400 {
		t.Fatal("unnamed snapshot accepted")
	}
	if c, _, raw := e.do("admin", "POST", base+"/snapshots", map[string]any{"name": "before patch", "description": "test"}); c != 200 {
		t.Fatalf("snapshot: %s", raw)
	}
	_, m, _ = e.do("viewer", "GET", base+"/config", nil)
	snaps := m["config"].(map[string]any)["snapshots"].([]any)
	if len(snaps) != 1 {
		t.Fatalf("snapshots: %v", snaps)
	}
	sref := snaps[0].(map[string]any)["ref"].(string)
	if c, _, _ := e.do("admin", "POST", base+"/snapshots/snapshot-999999/revert", nil); c != 404 {
		t.Fatal("unknown snapshot reverted")
	}
	if c, _, raw := e.do("admin", "POST", base+"/snapshots/"+sref+"/revert", nil); c != 200 {
		t.Fatalf("revert: %s", raw)
	}
	if c, _, raw := e.do("admin", "DELETE", base+"/snapshots/"+sref, nil); c != 200 {
		t.Fatalf("delete snapshot: %s", raw)
	}

	var audits int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='vsphere.vm_edit' AND target_id=$1`, vm.Ref).Scan(&audits)
	if audits < 9 {
		t.Fatalf("edits not audited: %d", audits)
	}
}
