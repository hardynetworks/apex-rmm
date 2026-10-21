package server

import (
	"strings"
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/vsphere"
)

func TestUUIDKey(t *testing.T) {
	const want = "42021e0b1a71ab6d3d8fc441224a5b99"
	for _, in := range []string{
		"VMware-42 02 1e 0b 1a 71 ab 6d-3d 8f c4 41 22 4a 5b 99",
		"42021e0b-1a71-ab6d-3d8f-c441224a5b99",
		"42021E0B-1A71-AB6D-3D8F-C441224A5B99",
	} {
		if got := uuidKey(in); got != want {
			t.Errorf("uuidKey(%q)=%q", in, got)
		}
	}
	for _, in := range []string{"", "To Be Filled By O.E.M.", "VMware-1234", "ABC123"} {
		if got := uuidKey(in); got != "" {
			t.Errorf("uuidKey(%q)=%q, want empty", in, got)
		}
	}
}

func TestMatchAgents(t *testing.T) {
	vms := []vsphere.VM{
		{Ref: "vm-1", Name: "dc01", UUID: "42021e0b-1a71-ab6d-3d8f-c441224a5b99", Hostname: "DC01.corp.local", IPs: []string{"10.0.0.5"}},
		{Ref: "vm-2", Name: "web", UUID: "11111111-1111-1111-1111-111111111111", Hostname: "web", IPs: []string{"10.0.0.20"}},
		{Ref: "vm-3", Name: "files", UUID: "22222222-2222-2222-2222-222222222222", Hostname: "files.corp.local", IPs: []string{"10.0.0.30"}},
		{Ref: "vm-4", Name: "no-agent", UUID: "33333333-3333-3333-3333-333333333333", Hostname: "lonely", IPs: []string{"10.0.0.40"}},
		{Ref: "vm-5", Name: "twin-a", UUID: "44444444-4444-4444-4444-444444444444", Hostname: "twin", IPs: nil},
		{Ref: "vm-6", Name: "twin-b", UUID: "55555555-5555-5555-5555-555555555555", Hostname: "twin", IPs: nil},
	}
	devs := []agentDev{
		{ID: "d-serial", Hostname: "something-else", Serial: "VMware-42 02 1e 0b 1a 71 ab 6d-3d 8f c4 41 22 4a 5b 99", IPs: []string{"192.168.1.1"}, Online: true},
		{ID: "d-ip", Hostname: "renamed", Serial: "VMware-Z", IPs: []string{"10.0.0.20", "10.0.0.21"}},
		{ID: "d-host", Hostname: "FILES", DisplayName: "File server", Serial: "", IPs: nil},
		{ID: "d-twin", Hostname: "twin", IPs: nil},
		{ID: "d-physical", Hostname: "laptop", Serial: "ABC123", IPs: []string{"10.0.0.99"}},
	}
	got := matchAgents(vms, devs)
	check := func(ref, dev, how string) {
		t.Helper()
		l, ok := got[ref]
		if !ok || l.DeviceID != dev || l.Match != how {
			t.Errorf("%s: got %+v, want device %s via %s", ref, l, dev, how)
		}
	}
	check("vm-1", "d-serial", "uuid") // the UUID wins even though host name and IP differ
	check("vm-2", "d-ip", "ip")
	check("vm-3", "d-host", "hostname")
	if got["vm-3"].Name != "File server" {
		t.Errorf("display name should be preferred: %+v", got["vm-3"])
	}
	if _, ok := got["vm-4"]; ok {
		t.Error("a VM with no agent must not be linked")
	}
	if _, ok := got["vm-5"]; ok {
		t.Error("two VMs sharing a host name are ambiguous and must not be linked")
	}
	if _, ok := got["vm-6"]; ok {
		t.Error("two VMs sharing a host name are ambiguous and must not be linked")
	}
	seen := map[string]bool{}
	for ref, l := range got {
		if seen[l.DeviceID] {
			t.Errorf("device %s linked to more than one VM (%s)", l.DeviceID, ref)
		}
		seen[l.DeviceID] = true
	}
}

func testConn() *vsConn {
	return &vsConn{ID: "c1", Name: "lab-vc", DSWarnPct: 85, DSCritPct: 95, SnapshotDays: 3, AlertAlarms: true, AlertHosts: true}
}

func keys(as []srcAlert) map[string]srcAlert {
	m := map[string]srcAlert{}
	for _, a := range as {
		m[a.Key] = a
	}
	return m
}

func TestVSphereAlerts(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-5 * 24 * time.Hour)
	fresh := now.Add(-2 * time.Hour)
	inv := &vsphere.Inventory{
		CollectedAt: now,
		Hosts: []vsphere.Host{
			{Ref: "host-1", Name: "esxi01", ConnectionState: "connected"},
			{Ref: "host-2", Name: "esxi02", ConnectionState: "notResponding"},
			{Ref: "host-3", Name: "esxi03", ConnectionState: "disconnected"},
			{Ref: "host-4", Name: "esxi04", ConnectionState: "connected", Sensors: []vsphere.Sensor{
				{Name: "PSU 2", Type: "power", State: "red"}, {Name: "Fan 3", Type: "fan", State: "yellow", Reading: "4200.0 RPM"}}},
		},
		Datastores: []vsphere.Datastore{
			{Ref: "ds-1", Name: "ok", Capacity: 1000, Free: 500, Accessible: true},
			{Ref: "ds-2", Name: "warn", Capacity: 1000, Free: 100, Accessible: true},
			{Ref: "ds-3", Name: "crit", Capacity: 1000, Free: 30, Accessible: true},
			{Ref: "ds-4", Name: "gone", Accessible: false},
			{Ref: "ds-5", Name: "empty-capacity", Capacity: 0, Accessible: true},
		},
		VMs: []vsphere.VM{
			{Ref: "vm-1", Name: "old-snap", Snapshots: 2, OldestSnapshot: &old},
			{Ref: "vm-2", Name: "fresh-snap", Snapshots: 1, OldestSnapshot: &fresh},
			{Ref: "vm-3", Name: "tmpl", Template: true, Snapshots: 1, OldestSnapshot: &old},
		},
		Alarms: []vsphere.Alarm{
			{EntityType: "host", EntityRef: "host-1", EntityName: "esxi01", AlarmRef: "alarm-9", AlarmName: "Host memory usage", Status: "yellow", Time: now},
			{EntityType: "vm", EntityRef: "vm-1", EntityName: "old-snap", AlarmRef: "alarm-2", AlarmName: "VM CPU", Status: "red", Time: now},
			{EntityType: "vm", EntityRef: "vm-2", EntityName: "fresh-snap", AlarmRef: "alarm-3", AlarmName: "handled", Status: "red", Time: now, Acknowledged: true},
		},
	}
	c := testConn()
	got := keys(vsphereAlerts(c, inv))
	want := map[string]string{
		"vsphere:c1:host:host-2:conn":           "critical",
		"vsphere:c1:host:host-3:conn":           "warning",
		"vsphere:c1:host:host-4:hw:power:PSU 2": "critical",
		"vsphere:c1:host:host-4:hw:fan:Fan 3":   "warning",
		"vsphere:c1:ds:ds-2:space":              "warning",
		"vsphere:c1:ds:ds-3:space":              "critical",
		"vsphere:c1:ds:ds-4:access":             "critical",
		"vsphere:c1:vm:vm-1:snapshot":           "warning",
		"vsphere:c1:alarm:host:host-1:alarm-9":  "warning",
		"vsphere:c1:alarm:vm:vm-1:alarm-2":      "critical",
	}
	for k, sev := range want {
		a, ok := got[k]
		if !ok {
			t.Errorf("missing alert %s", k)
		} else if a.Sev != sev {
			t.Errorf("%s severity %s, want %s", k, a.Sev, sev)
		}
	}
	if len(got) != len(want) {
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("unexpected alert %s: %s", k, got[k].Title)
			}
		}
	}
	if a := got["vsphere:c1:ds:ds-3:space"]; !strings.Contains(a.Title, "97%") || a.Name != "crit" {
		t.Errorf("datastore alert wrong: %+v", a)
	}
	if a := got["vsphere:c1:vm:vm-1:snapshot"]; !strings.Contains(a.Title, "5 days") {
		t.Errorf("snapshot alert wrong: %+v", a)
	}

	// switching rules off removes the corresponding alerts
	c.AlertHosts, c.AlertAlarms, c.SnapshotDays = false, false, 0
	got = keys(vsphereAlerts(c, inv))
	for k := range got {
		if strings.Contains(k, ":host:") || strings.Contains(k, ":alarm:") || strings.Contains(k, "snapshot") {
			t.Errorf("rule is off but %s was raised", k)
		}
	}
	if len(got) != 3 { // the three datastore alerts always apply
		t.Errorf("expected only datastore alerts, got %d", len(got))
	}
}
