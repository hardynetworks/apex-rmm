package vsphere

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestVMConfigEditing(t *testing.T) {
	cfg, stop := startSim(t)
	defer stop()
	ctx := context.Background()
	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	inv, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, v := range inv.VMs {
		if !v.Template {
			ref = v.Ref
			break
		}
	}

	vc, err := c.VMConfig(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if vc.CPUs == 0 || vc.MemMB == 0 || len(vc.Disks) == 0 || len(vc.Nics) == 0 || len(vc.Networks) == 0 || len(vc.Datastores) == 0 {
		t.Fatalf("config: %+v", vc)
	}
	if vc.PowerState != "poweredOn" {
		t.Fatalf("simulator VMs start powered on, got %s", vc.PowerState)
	}

	// validation: hot add is off in the simulator, so CPU/memory changes need the VM off
	two, mem := int32(2), int32(2048)
	if err := (VMSettings{CPUs: &two}).Validate(vc); err == nil || !strings.Contains(err.Error(), "hot add") {
		t.Fatalf("CPU change on a running VM without hot add: %v", err)
	}
	three := int32(3)
	cps := int32(2)
	off := *vc
	off.PowerState = "poweredOff"
	if err := (VMSettings{CPUs: &three, CoresPerSocket: &cps}).Validate(&off); err == nil {
		t.Fatal("3 vCPUs in sockets of 2 accepted")
	}
	odd := int32(1001)
	if err := (VMSettings{MemMB: &odd}).Validate(&off); err == nil {
		t.Fatal("memory not a multiple of 4 accepted")
	}

	if err := c.PowerVM(ctx, ref, "off"); err != nil {
		t.Fatal(err)
	}
	name, note := "renamed-vm", "Edited by Apex"
	s := VMSettings{Name: &name, Annotation: &note, CPUs: &two, MemMB: &mem}
	vc, _ = c.VMConfig(ctx, ref)
	if err := s.Validate(vc); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureVM(ctx, ref, s); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if vc.Name != name || vc.Annotation != note || vc.CPUs != 2 || vc.MemMB != 2048 {
		t.Fatalf("after configure: %s %q %d %d", vc.Name, vc.Annotation, vc.CPUs, vc.MemMB)
	}

	// disks: grow, add, remove
	d0 := vc.Disks[0]
	if err := c.ResizeDisk(ctx, ref, d0.Key, d0.SizeBytes-1); err == nil {
		t.Fatal("disk shrink accepted")
	}
	if err := c.ResizeDisk(ctx, ref, d0.Key, d0.SizeBytes+(1<<30)); err != nil {
		t.Fatal(err)
	}
	ds := vc.Datastores[0].Ref
	if err := c.AddDisk(ctx, ref, 2<<30, ds, true); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDisk(ctx, ref, 1<<30, ds, true); err != nil { // a second one gets its own file name
		t.Fatal(err)
	}
	if vc2, _ := c.VMConfig(ctx, ref); len(vc2.Disks) != 3 || vc2.Disks[1].File == vc2.Disks[2].File || !strings.HasSuffix(vc2.Disks[1].File, ".vmdk") || strings.HasPrefix(vc2.Disks[1].File, "["+vc.Datastores[0].Name+"].") {
		t.Fatalf("new disk files: %+v", vc2.Disks)
	} else if err := c.RemoveDisk(ctx, ref, vc2.Disks[2].Key, true); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if len(vc.Disks) != 2 || vc.Disks[0].SizeBytes != d0.SizeBytes+(1<<30) || vc.Disks[1].SizeBytes != 2<<30 || !vc.Disks[1].Thin {
		t.Fatalf("disks: %+v", vc.Disks)
	}
	if err := c.RemoveDisk(ctx, ref, vc.Disks[1].Key, true); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if len(vc.Disks) != 1 {
		t.Fatalf("disk not removed: %+v", vc.Disks)
	}

	// network adapter: disconnect, move to another network
	nic := vc.Nics[0]
	no := false
	target := vc.Networks[len(vc.Networks)-1]
	if err := c.SetNic(ctx, ref, nic.Key, target.Ref, &no); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if vc.Nics[0].Connected || vc.Nics[0].NetRef != target.Ref {
		t.Fatalf("nic: %+v (want %s)", vc.Nics[0], target.Ref)
	}

	// CD/DVD: mount an ISO found on the datastore, then eject
	if len(vc.Cdroms) > 0 {
		dsName := vc.Datastores[0].Name
		dir := dsDir(t, c, ds)
		_ = os.MkdirAll(filepath.Join(dir, "iso"), 0o755)
		if err := os.WriteFile(filepath.Join(dir, "iso", "ubuntu.iso"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		isos, err := c.ListISOs(ctx, ds)
		if err != nil {
			t.Fatal(err)
		}
		found := ""
		for _, f := range isos {
			if f.Name == "ubuntu.iso" {
				found = f.Path
			}
		}
		if found != "["+dsName+"] iso/ubuntu.iso" {
			t.Fatalf("iso search: %+v", isos)
		}
		if err := c.SetCdrom(ctx, ref, vc.Cdroms[0].Key, found, nil); err != nil {
			t.Fatal(err)
		}
		vc, _ = c.VMConfig(ctx, ref)
		if vc.Cdroms[0].ISO != found || !vc.Cdroms[0].OnBoot { // powered off: connects at power on
			t.Fatalf("cd: %+v", vc.Cdroms[0])
		}
		if err := c.SetCdrom(ctx, ref, vc.Cdroms[0].Key, "", nil); err != nil {
			t.Fatal(err)
		}
		vc, _ = c.VMConfig(ctx, ref)
		if vc.Cdroms[0].ISO != "" {
			t.Fatalf("cd not ejected: %+v", vc.Cdroms[0])
		}
	}

	// snapshots: take two, revert to the first, delete
	if err := c.TakeSnapshot(ctx, ref, "before-update", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := c.TakeSnapshot(ctx, ref, "after-update", "second", false, false); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if len(vc.Snapshots) != 2 || vc.Snapshots[1].Parent != vc.Snapshots[0].Ref || !vc.Snapshots[1].Current || vc.Snapshots[1].Depth != 1 {
		t.Fatalf("snapshots: %+v", vc.Snapshots)
	}
	if err := c.RevertSnapshot(ctx, vc.Snapshots[0].Ref); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if !vc.Snapshots[0].Current {
		t.Fatalf("revert: %+v", vc.Snapshots)
	}
	if err := c.DeleteSnapshot(ctx, vc.Snapshots[0].Ref, true); err != nil {
		t.Fatal(err)
	}
	vc, _ = c.VMConfig(ctx, ref)
	if len(vc.Snapshots) != 0 {
		t.Fatalf("snapshots left: %+v", vc.Snapshots)
	}
}

// dsDir is the local folder that backs a simulator datastore.
func dsDir(t *testing.T, c *Conn, ref string) string {
	t.Helper()
	var d mo.Datastore
	if err := property.DefaultCollector(c.c.Client).RetrieveOne(context.Background(), types.ManagedObjectReference{Type: "Datastore", Value: ref}, []string{"summary"}, &d); err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(d.Summary.Url, "ds://")
}
