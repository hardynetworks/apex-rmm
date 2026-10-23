package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
	"github.com/hardynetworks/apex-rmm/internal/tapo/tapotest"
)

// Tapo plugs end to end against a fake P115: account, adding by IP, assignment to a device and
// to a vSphere host (its watts replace theirs), switching, errors and removal.
func TestTapoPlugs(t *testing.T) {
	e := newITEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = e.s.db.Exec(ctx, `DELETE FROM tapo_plugs`)
	_, _ = e.s.db.Exec(ctx, `DELETE FROM matter_plugs`)
	_ = e.s.db.SetSetting(ctx, "tapo", TapoSettings{})
	e.s.tp.start(ctx)

	f := tapotest.New("cade@example.com", "Tapo-pass-1")
	defer f.Close()

	if c, _, _ := e.do("admin", "POST", "/api/tapo/plugs", map[string]any{"host": f.Host()}); c != 400 {
		t.Fatalf("plug added without an account: %d", c)
	}
	if c, _, _ := e.do("technician", "PUT", "/api/tapo/settings", map[string]any{"username": "x", "password": "y"}); c != 403 {
		t.Fatalf("technician saved the account: %d", c)
	}
	if c, _, _ := e.do("admin", "PUT", "/api/tapo/settings", map[string]any{"username": "cade@example.com"}); c != 400 {
		t.Fatal("account without password accepted")
	}
	// a wrong password first: adding the plug explains the problem
	if c, _, raw := e.do("admin", "PUT", "/api/tapo/settings", map[string]any{"username": "cade@example.com", "password": "wrong"}); c != 200 {
		t.Fatalf("settings: %d %s", c, raw)
	}
	if c, _, raw := e.do("admin", "POST", "/api/tapo/plugs", map[string]any{"host": f.Host()}); c != 400 || !strings.Contains(string(raw), "case-sensitive") {
		t.Fatalf("wrong password: %d %s", c, raw)
	}
	if c, _, _ := e.do("admin", "PUT", "/api/tapo/settings", map[string]any{"username": "cade@example.com", "password": "Tapo-pass-1"}); c != 200 {
		t.Fatal("settings")
	}
	// the password is never sent back and is stored sealed
	_, m, raw := e.do("viewer", "GET", "/api/tapo", nil)
	if m["configured"] != true || strings.Contains(string(raw), "Tapo-pass-1") {
		t.Fatalf("GET /api/tapo: %s", raw)
	}
	var stored TapoSettings
	e.s.db.Setting(ctx, "tapo", &stored)
	if stored.Password == "Tapo-pass-1" || stored.Password == "" {
		t.Fatalf("password stored as %q", stored.Password)
	}
	// saving with an empty password keeps it
	if c, _, _ := e.do("admin", "PUT", "/api/tapo/settings", map[string]any{"username": "cade@example.com"}); c != 200 {
		t.Fatal("keep password")
	}

	if c, _, _ := e.do("admin", "POST", "/api/tapo/plugs", map[string]any{"host": "http://x/y"}); c != 400 {
		t.Fatal("bad host accepted")
	}
	c, pm, raw := e.do("admin", "POST", "/api/tapo/plugs", map[string]any{"host": f.Host()})
	if c != 200 || pm["name"] != "Rack plug" || pm["model"] != "P115" || pm["energy"] != true {
		t.Fatalf("add: %d %s", c, raw)
	}
	id := pm["id"].(string)
	if c, _, _ := e.do("admin", "POST", "/api/tapo/plugs", map[string]any{"host": f.Host()}); c != 400 {
		t.Fatal("same plug added twice")
	}
	waitLive := func(cond func(p map[string]any) bool) map[string]any {
		for i := 0; ; i++ {
			_, m, _ := e.do("viewer", "GET", "/api/tapo", nil)
			if ps, _ := m["plugs"].([]any); len(ps) == 1 {
				if p, _ := ps[0].(map[string]any)["plug"].(map[string]any); p != nil && cond(p) {
					return ps[0].(map[string]any)
				}
			}
			if i > 150 {
				t.Fatalf("plug never reached the expected state: %v", m["plugs"])
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	p := waitLive(func(p map[string]any) bool { return p["available"] == true })
	if l := p["plug"].(map[string]any); l["watts"] != 182.4 || l["product"] != "P115" || l["volts"] != 121.5 || l["month_kwh"] != 45.678 {
		t.Fatalf("live: %v", l)
	}

	// assign to a device: its watts replace the agent's estimate
	var dev string
	if err := e.s.db.QueryRow(ctx, `INSERT INTO devices (hostname, secret_hash) VALUES ('tapobox', 'x') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if c, _, raw := e.do("admin", "PATCH", "/api/tapo/plugs/"+id, map[string]any{"device_id": dev}); c != 200 {
		t.Fatalf("assign: %d %s", c, raw)
	}
	e.s.recordMetrics(ctx, dev, &proto.Metrics{Energy: &proto.Energy{Watts: 40, Source: "estimate", Kind: "estimate"}})
	var w float64
	var src, kind string
	_ = e.s.db.QueryRow(ctx, `SELECT power_w, power_src, power_kind FROM devices WHERE id=$1`, dev).Scan(&w, &src, &kind)
	if w < 182.3 || w > 182.5 || src != "tapo" || kind != "measured" {
		t.Fatalf("device power = %v %s %s", w, src, kind)
	}
	_, m, _ = e.do("viewer", "GET", "/api/tapo?device="+dev, nil)
	if ps, _ := m["plugs"].([]any); len(ps) != 1 || ps[0].(map[string]any)["device_name"] != "tapobox" {
		t.Fatalf("device filter: %v", m["plugs"])
	}

	// assign to a vSphere host instead
	var clientID string
	_ = e.s.db.QueryRow(ctx, `SELECT id FROM clients LIMIT 1`).Scan(&clientID)
	_, cm, _ := e.do("admin", "POST", "/api/vsphere/test", map[string]any{"url": e.sim, "username": "user", "password": "pass"})
	_, vm, raw := e.do("admin", "POST", "/api/vsphere/connections", map[string]any{"name": "tapo-lab", "url": e.sim, "username": "user", "password": "pass",
		"thumbprint": cm["thumbprint"], "enabled": true, "poll_seconds": 300, "ds_warn_pct": 85, "ds_crit_pct": 95, "snapshot_days": 3, "client_id": clientID})
	conn, _ := vm["id"].(string)
	if conn == "" {
		t.Fatalf("connection: %s", raw)
	}
	e.pollNow(conn)
	inv, _, _ := e.s.loadInventory(ctx, conn)
	ref := inv.Hosts[0].Ref
	if c, _, raw := e.do("admin", "PATCH", "/api/tapo/plugs/"+id, map[string]any{"vs_conn": conn, "host_ref": ref}); c != 200 {
		t.Fatalf("assign host: %d %s", c, raw)
	}
	f.SetPower(351.2)
	e.s.tp.poke()
	waitLive(func(p map[string]any) bool { return p["watts"] == 351.2 })
	e.pollNow(conn)
	inv, _, _ = e.s.loadInventory(ctx, conn)
	if h := inv.Hosts[0]; h.PowerWatts == nil || *h.PowerWatts != 351.2 || h.PowerSource != "plug" || h.PowerNote != "Tapo P115" {
		t.Fatalf("host power: %v %q %q", h.PowerWatts, h.PowerSource, h.PowerNote)
	}
	// the device is no longer powered by it
	e.s.recordMetrics(ctx, dev, &proto.Metrics{Energy: &proto.Energy{Watts: 40, Source: "estimate", Kind: "estimate"}})
	_ = e.s.db.QueryRow(ctx, `SELECT power_w, power_src FROM devices WHERE id=$1`, dev).Scan(&w, &src)
	if w != 40 || src != "estimate" {
		t.Fatalf("device still uses the plug: %v %s", w, src)
	}

	// switching: admins only, audited
	if c, _, _ := e.do("technician", "POST", "/api/tapo/plugs/"+id+"/power", map[string]any{"action": "off"}); c != 403 {
		t.Fatalf("technician switched a plug: %d", c)
	}
	if c, _, raw := e.do("admin", "POST", "/api/tapo/plugs/"+id+"/power", map[string]any{"action": "cycle", "seconds": 1}); c != 200 {
		t.Fatalf("cycle: %d %s", c, raw)
	}
	if f.On() {
		t.Fatal("plug not switched off")
	}
	for i := 0; !f.On(); i++ {
		if i > 100 {
			t.Fatal("plug not switched back on")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var audits int
	_ = e.s.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='tapo.power'`).Scan(&audits)
	if audits != 1 {
		t.Fatalf("power cycle not audited: %d", audits)
	}

	// Third-Party Compatibility switched off on the plug: the error says what to do
	f.SetRefuse(true)
	f.DropSessions()
	e.s.tp.poke()
	waitLive(func(p map[string]any) bool {
		s, _ := p["error"].(string)
		return strings.Contains(s, "Third-Party Compatibility")
	})
	f.SetRefuse(false)

	// rename, remove
	if c, _, _ := e.do("admin", "PATCH", "/api/tapo/plugs/"+id, map[string]any{"name": " "}); c != 400 {
		t.Fatal("empty name accepted")
	}
	if c, _, _ := e.do("admin", "PATCH", "/api/tapo/plugs/"+id, map[string]any{"name": "ESXi 102"}); c != 200 {
		t.Fatal("rename")
	}
	if c, _, _ := e.do("admin", "DELETE", "/api/tapo/plugs/"+id, nil); c != 200 {
		t.Fatal("delete")
	}
	if c, _, _ := e.do("admin", "DELETE", "/api/tapo/plugs/"+id, nil); c != 404 {
		t.Fatal("deleted twice")
	}
	if _, _, ok := e.s.tp.watts("", conn, ref); ok {
		t.Fatal("removed plug still powers the host")
	}
}
