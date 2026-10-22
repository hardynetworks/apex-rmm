package server

import (
	"context"
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/matter/mattertest"
	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// Matter plugs end to end against a fake controller: settings, pairing, assigning a plug to a
// device and to a vSphere host (its watts replace theirs), switching and removing.
func TestMatterPlugs(t *testing.T) {
	e := newITEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = e.s.db.Exec(ctx, `DELETE FROM matter_plugs`)
	_ = e.s.db.SetSetting(ctx, "matter", MatterSettings{})
	e.s.mt.start(ctx)

	f := mattertest.New()
	defer f.Close()
	f.AddPairingCode("34970112332", map[string]any{"0/40/1": "Linkind", "0/40/3": "Matter Smart Plug", "1/6/0": true})
	f.AddPairingCode("MT:Y.K9042C00KA0648G00", map[string]any{"0/40/1": "TP-Link", "0/40/3": "KP125M", "1/6/0": true, "1/144/8": 182400})

	code, m, _ := e.do("viewer", "GET", "/api/matter", nil)
	if code != 200 || m["enabled"] != false || m["url"] != defaultMatterURL {
		t.Fatalf("matter off by default: %d %v", code, m)
	}
	if c, _, _ := e.do("technician", "PUT", "/api/matter/settings", map[string]any{"enabled": true, "url": f.URL()}); c != 403 {
		t.Fatalf("technician changed Matter settings: %d", c)
	}
	if c, _, _ := e.do("admin", "PUT", "/api/matter/settings", map[string]any{"enabled": true, "url": "http://x"}); c != 400 {
		t.Fatalf("non-ws URL accepted: %d", c)
	}
	if c, _, raw := e.do("admin", "PUT", "/api/matter/settings", map[string]any{"enabled": true, "url": f.URL()}); c != 200 {
		t.Fatalf("settings: %d %s", c, raw)
	}
	for i := 0; ; i++ {
		_, m, _ = e.do("viewer", "GET", "/api/matter", nil)
		if st, _ := m["status"].(map[string]any); st["state"] == "connected" {
			break
		}
		if i > 100 {
			t.Fatalf("never connected: %v", m["status"])
		}
		time.Sleep(30 * time.Millisecond)
	}

	// pairing
	if c, _, _ := e.do("admin", "POST", "/api/matter/plugs", map[string]any{"code": "12"}); c != 400 {
		t.Fatalf("bad code accepted: %d", c)
	}
	if c, m, _ := e.do("admin", "POST", "/api/matter/plugs", map[string]any{"code": "1111-111-1111"}); c != 400 {
		t.Fatalf("unknown device should fail: %d %v", c, m)
	}
	c, lm, raw := e.do("admin", "POST", "/api/matter/plugs", map[string]any{"code": "3497-011-2332"})
	if c != 200 || lm["name"] != "Linkind Matter Smart Plug" {
		t.Fatalf("pair linkind: %d %s", c, raw)
	}
	linkind := int64(lm["node_id"].(float64))
	c, km, raw := e.do("admin", "POST", "/api/matter/plugs", map[string]any{"code": "MT:Y.K9042C00KA0648G00", "name": "Rack plug"})
	if c != 200 || km["name"] != "Rack plug" {
		t.Fatalf("pair kasa: %d %s", c, raw)
	}
	kasa := int64(km["node_id"].(float64))
	_, m, _ = e.do("viewer", "GET", "/api/matter", nil)
	if ps, _ := m["plugs"].([]any); len(ps) != 2 {
		t.Fatalf("plugs: %v", m["plugs"])
	}

	// assign the measuring plug to a device: its watts replace the agent's estimate
	var dev string
	if err := e.s.db.QueryRow(ctx, `INSERT INTO devices (hostname, secret_hash) VALUES ('plugbox', 'x') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if c, _, raw := e.do("admin", "PATCH", "/api/matter/plugs/"+itoa(kasa), map[string]any{"device_id": dev}); c != 200 {
		t.Fatalf("assign: %d %s", c, raw)
	}
	e.s.recordMetrics(ctx, dev, &proto.Metrics{Energy: &proto.Energy{Watts: 50, Source: "estimate", Kind: "estimate"}})
	var w float64
	var src, kind string
	_ = e.s.db.QueryRow(ctx, `SELECT power_w, power_src, power_kind FROM devices WHERE id=$1`, dev).Scan(&w, &src, &kind)
	if w < 182.3 || w > 182.5 || src != "matter" || kind != "measured" { // real column
		t.Fatalf("device power = %v %s %s", w, src, kind)
	}
	_, m, _ = e.do("viewer", "GET", "/api/matter?device="+dev, nil)
	if ps, _ := m["plugs"].([]any); len(ps) != 1 || ps[0].(map[string]any)["device_name"] != "plugbox" {
		t.Fatalf("device filter: %v", m["plugs"])
	}
	// a plug without power measurement leaves the agent's reading alone
	if c, _, _ := e.do("admin", "PATCH", "/api/matter/plugs/"+itoa(linkind), map[string]any{"device_id": dev}); c != 200 {
		t.Fatal("reassign")
	}
	e.s.recordMetrics(ctx, dev, &proto.Metrics{Energy: &proto.Energy{Watts: 50, Source: "estimate", Kind: "estimate"}})
	_ = e.s.db.QueryRow(ctx, `SELECT power_w, power_src FROM devices WHERE id=$1`, dev).Scan(&w, &src)
	if w != 50 || src != "estimate" {
		t.Fatalf("on/off-only plug changed the power: %v %s", w, src)
	}
	var onDev int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM matter_plugs WHERE device_id=$1`, dev).Scan(&onDev)
	if onDev != 1 {
		t.Fatalf("a device should have one plug, has %d", onDev)
	}

	// assign the measuring plug to a vSphere host: the host's power comes from it
	var clientID string
	_ = e.s.db.QueryRow(ctx, `SELECT id FROM clients LIMIT 1`).Scan(&clientID)
	_, cm, _ := e.do("admin", "POST", "/api/vsphere/test", map[string]any{"url": e.sim, "username": "user", "password": "pass"})
	_, vm, raw := e.do("admin", "POST", "/api/vsphere/connections", map[string]any{"name": "plugs-lab", "url": e.sim, "username": "user", "password": "pass",
		"thumbprint": cm["thumbprint"], "enabled": true, "poll_seconds": 300, "ds_warn_pct": 85, "ds_crit_pct": 95, "snapshot_days": 3, "client_id": clientID})
	conn, _ := vm["id"].(string)
	if conn == "" {
		t.Fatalf("connection: %s", raw)
	}
	e.pollNow(conn)
	inv, _, _ := e.s.loadInventory(ctx, conn)
	ref := inv.Hosts[0].Ref
	if c, _, raw := e.do("admin", "PATCH", "/api/matter/plugs/"+itoa(kasa), map[string]any{"vs_conn": conn, "host_ref": ref}); c != 200 {
		t.Fatalf("assign host: %d %s", c, raw)
	}
	e.pollNow(conn)
	inv, _, _ = e.s.loadInventory(ctx, conn)
	if h := inv.Hosts[0]; h.PowerWatts == nil || *h.PowerWatts != 182.4 || h.PowerSource != "plug" {
		t.Fatalf("host power: %v %q", h.PowerWatts, h.PowerSource)
	}
	_, m, _ = e.do("viewer", "GET", "/api/matter", nil)
	for _, x := range m["plugs"].([]any) {
		p := x.(map[string]any)
		if int64(p["node_id"].(float64)) == kasa && (p["host_name"] != inv.Hosts[0].Name || p["device_id"] != nil) {
			t.Fatalf("host assignment shown wrong: %v", p)
		}
	}

	// switching: admins only
	if c, _, _ := e.do("technician", "POST", "/api/matter/plugs/"+itoa(kasa)+"/power", map[string]any{"action": "off"}); c != 403 {
		t.Fatalf("technician switched a plug: %d", c)
	}
	if c, _, raw := e.do("admin", "POST", "/api/matter/plugs/"+itoa(kasa)+"/power", map[string]any{"action": "cycle", "seconds": 1}); c != 200 {
		t.Fatalf("cycle: %d %s", c, raw)
	}
	if f.Attr(kasa, "1/6/0") != false {
		t.Fatal("plug not switched off")
	}
	for i := 0; f.Attr(kasa, "1/6/0") != true; i++ {
		if i > 100 {
			t.Fatal("plug not switched back on")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var audits int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='matter.power'`).Scan(&audits)
	if audits != 1 {
		t.Fatalf("power cycle not audited: %d", audits)
	}

	// rename, remove
	if c, _, _ := e.do("admin", "PATCH", "/api/matter/plugs/"+itoa(linkind), map[string]any{"name": " "}); c != 400 {
		t.Fatal("empty name accepted")
	}
	if c, _, _ := e.do("admin", "DELETE", "/api/matter/plugs/"+itoa(linkind), nil); c != 200 {
		t.Fatal("delete")
	}
	if f.Attr(linkind, "0/40/1") != nil {
		t.Fatal("plug not removed from the fabric")
	}
	if c, _, _ := e.do("admin", "PATCH", "/api/matter/plugs/"+itoa(linkind), map[string]any{"name": "x"}); c != 404 {
		t.Fatal("deleted plug still there")
	}
}

func itoa(v int64) string {
	b := []byte{}
	if v == 0 {
		return "0"
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
