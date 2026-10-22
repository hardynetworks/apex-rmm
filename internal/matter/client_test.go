package matter_test

import (
	"context"
	"testing"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/matter"
	"github.com/hardynetworks/apex-rmm/internal/matter/mattertest"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

func TestClient(t *testing.T) {
	f := mattertest.New()
	defer f.Close()
	// an energy-monitoring plug already on the fabric, and an on/off-only plug to commission
	meter := f.AddNode(map[string]any{
		"0/40/1": "TP-Link", "0/40/3": "KP125M", "0/40/15": "SN1",
		"1/6/0": true, "1/144/8": 182400, "1/144/4": 120100, "1/144/5": 1520,
		"1/145/1": map[string]any{"energy": 12500000},
	})
	f.AddPairingCode("34970112332", map[string]any{"0/40/1": "Linkind", "0/40/3": "Matter Smart Plug", "1/6/0": false})

	c := matter.New(f.URL())
	updates := make(chan int64, 100)
	c.OnUpdate = func(id int64) { updates <- id }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, "connected", func() bool { return c.Status().State == "connected" })

	p, ok := c.Plug(meter)
	if !ok || p.Vendor != "TP-Link" || p.Product != "KP125M" || p.Endpoint != 1 || p.On == nil || !*p.On ||
		p.Watts == nil || *p.Watts != 182.4 || *p.Volts != 120.1 || *p.Amps != 1.52 || *p.EnergyKWh != 12.5 {
		t.Fatalf("%+v", p)
	}

	// live updates
	f.SetAttr(meter, "1/144/8", 95000)
	waitFor(t, "power update", func() bool { p, _ := c.Plug(meter); return p.Watts != nil && *p.Watts == 95 })

	// commission with a manual code typed with dashes
	lp, err := c.Commission(ctx, "3497-011-2332")
	if err != nil || lp.Vendor != "Linkind" || lp.Watts != nil || lp.On == nil || *lp.On {
		t.Fatalf("commission: %v %+v", err, lp)
	}
	if _, err := c.Commission(ctx, "11111111111"); err == nil {
		t.Fatal("unknown code should fail")
	}

	// switching
	if err := c.SetOn(ctx, lp.NodeID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "on", func() bool { p, _ := c.Plug(lp.NodeID); return p.On != nil && *p.On })
	if err := c.SetOn(ctx, meter, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "off and 0 W", func() bool { p, _ := c.Plug(meter); return p.On != nil && !*p.On && p.Watts != nil && *p.Watts == 0 })

	// removing
	if err := c.Remove(ctx, lp.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Plug(lp.NodeID); ok || len(c.Plugs()) != 1 {
		t.Fatal("node still there")
	}

	// reconnects after the controller restarts the connection
	f.CloseClientConnections()
	waitFor(t, "reconnected", func() bool { return c.Status().State == "connected" && len(c.Plugs()) == 1 })
	if len(updates) == 0 {
		t.Fatal("no update callbacks")
	}
}

func TestNotConnected(t *testing.T) {
	c := matter.New("ws://127.0.0.1:1/ws")
	if _, err := c.Commission(context.Background(), "123"); err != matter.ErrNotConnected {
		t.Fatal(err)
	}
}
