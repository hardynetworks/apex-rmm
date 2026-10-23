package tapo_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/hardynetworks/apex-rmm/internal/tapo"
	"github.com/hardynetworks/apex-rmm/internal/tapo/tapotest"
)

func TestReadSwitchAndRelogin(t *testing.T) {
	ctx := context.Background()
	f := tapotest.New("me@example.com", "Secret-123")
	defer f.Close()
	c := tapo.New(f.Host(), tapo.Credentials{Username: "me@example.com", Password: "Secret-123"})
	defer c.Close()

	st, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Model != "P115" || st.Nickname != "Rack plug" || !st.On || st.Watts == nil || *st.Watts != 182.4 ||
		st.Volts == nil || *st.Volts != 121.5 || st.TodayKWh == nil || *st.TodayKWh != 1.234 || *st.MonthKWh != 45.678 {
		t.Fatalf("state: %+v", st)
	}
	if err := c.SetOn(ctx, false); err != nil || f.On() {
		t.Fatalf("switch off: %v on=%v", err, f.On())
	}
	if f.Handshake != 1 {
		t.Fatalf("one login expected, got %d", f.Handshake)
	}
	// the plug restarts: the client logs in again by itself
	f.DropSessions()
	if err := c.SetOn(ctx, true); err != nil || !f.On() {
		t.Fatalf("after restart: %v", err)
	}
	if f.Handshake != 2 {
		t.Fatalf("relogin expected, got %d", f.Handshake)
	}
}

func TestNoEnergyMonitoring(t *testing.T) {
	f := tapotest.New("a@b.c", "p")
	defer f.Close()
	f.Model, f.Energy = "P100", false
	st, err := tapo.New(f.Host(), tapo.Credentials{Username: "a@b.c", Password: "p"}).Read(context.Background())
	if err != nil || st.Model != "P100" || st.Watts != nil {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	f := tapotest.New("me@example.com", "right")
	defer f.Close()
	if _, err := tapo.New(f.Host(), tapo.Credentials{Username: "me@example.com", Password: "wrong"}).Read(ctx); !errors.Is(err, tapo.ErrAuth) {
		t.Fatalf("wrong password: %v", err)
	}
	f.Refuse = true
	if _, err := tapo.New(f.Host(), tapo.Credentials{Username: "me@example.com", Password: "right"}).Read(ctx); !errors.Is(err, tapo.ErrRefused) {
		t.Fatalf("TPAP plug: %v", err)
	}
	// plugs never linked to the cloud take blank credentials
	g := tapotest.New("", "")
	defer g.Close()
	if _, err := tapo.New(g.Host(), tapo.Credentials{Username: "me@example.com", Password: "x"}).Read(ctx); err != nil {
		t.Fatalf("blank credentials fallback: %v", err)
	}
}

// Vectors generated with python-kasa's KlapTransportV2 / KlapEncryptionSession.
func TestPythonKasaVectors(t *testing.T) {
	local, remote := make([]byte, 16), make([]byte, 16)
	for i := range local {
		local[i], remote[i] = byte(i), byte(100+i)
	}
	auth, hs1, hs2, seq1, p1, seq2, p2 := tapo.Vectors(local, remote, "me@example.com", "Secret-123",
		[]byte(`{"method":"get_device_info","params":null}`), []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"))
	want := map[string]string{
		"auth": "0eda10b56ba3c7ae954af81d94fb9d5b79e43543fd7d8540384c4354e4c20c66",
		"hs1":  "aeacc8b0b5cf15988a42c19102220b3e78ca8ded792a022f58c2c32e30949536",
		"hs2":  "25a49f373d6111587d0b5be097a2a53b7112347e77189c91c597cac548da4833",
		"p1":   "ea5e039045039c9fab545c8c928f0cf5eeac71f92a2b37a696b4308426ff5b6fdd9ab5e3c8d97124cf164c58a48a74e8f2460aac60bad88efb10ec9c62295bef756a8274571aec80bdff3052fe732bc2",
		"p2":   "a0f1e7291b6814a856ae8143e742ca7a0e06f15048878a14d8de55a6e597ce2efa4a671f97f5a1d54c197762f9fb44fb66adb2b139da5b42adaddd56842db055b8f0479598464602569c555893290a2e",
	}
	got := map[string]string{"auth": hex.EncodeToString(auth), "hs1": hex.EncodeToString(hs1), "hs2": hex.EncodeToString(hs2),
		"p1": hex.EncodeToString(p1), "p2": hex.EncodeToString(p2)}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s, want %s", k, got[k], v)
		}
	}
	if seq1 != 1260713325 || seq2 != 1260713326 {
		t.Errorf("seq %d %d", seq1, seq2)
	}
}
