package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/tapo"
)

// TP-Link Tapo Wi-Fi plugs (P110, P115…). The Apex server polls each plug on the local network
// every few seconds with the TP-Link account they were set up with. Like Matter plugs, a Tapo
// plug can be assigned to an agent device or a vSphere host, and its watts replace theirs.

const tapoPoll = 10 * time.Second

// TapoSettings are stored under the "tapo" setting; the password is sealed with the app key.
type TapoSettings struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TapoLive is what the dashboard sees of a plug's last poll.
type TapoLive struct {
	Available bool     `json:"available"`
	Vendor    string   `json:"vendor"`
	Product   string   `json:"product"`
	Firmware  string   `json:"firmware,omitempty"`
	RSSI      int      `json:"rssi,omitempty"`
	On        *bool    `json:"on"`
	Watts     *float64 `json:"watts"`
	Volts     *float64 `json:"volts"`
	Amps      *float64 `json:"amps"`
	TodayKWh  *float64 `json:"today_kwh"`
	MonthKWh  *float64 `json:"month_kwh"`
	Error     string   `json:"error,omitempty"`
	At        string   `json:"at,omitempty"`
}

type tapoPlug struct {
	id, host string
	target   plugTarget
	client   *tapo.Client
	live     TapoLive
	seen     time.Time // last good read
}

type tapoMgr struct {
	s     *Server
	mu    sync.Mutex
	creds tapo.Credentials
	plugs map[string]*tapoPlug
	kick  chan struct{}
}

func newTapoMgr(s *Server) *tapoMgr {
	return &tapoMgr{s: s, plugs: map[string]*tapoPlug{}, kick: make(chan struct{}, 1)}
}

func (m *tapoMgr) settings(ctx context.Context) (TapoSettings, tapo.Credentials) {
	var st TapoSettings
	m.s.db.Setting(ctx, "tapo", &st)
	pw, _ := m.s.box.open(st.Password)
	return st, tapo.Credentials{Username: st.Username, Password: pw}
}

func (m *tapoMgr) start(ctx context.Context) {
	m.reload(ctx)
	go func() {
		t := time.NewTicker(tapoPoll)
		defer t.Stop()
		for {
			m.pollAll(ctx)
			select {
			case <-ctx.Done():
				m.mu.Lock()
				for _, p := range m.plugs {
					p.client.Close()
				}
				m.mu.Unlock()
				return
			case <-t.C:
			case <-m.kick:
			}
		}
	}()
}

// reload re-reads the plugs and the account; clients are kept when nothing about them changed.
func (m *tapoMgr) reload(ctx context.Context) {
	_, creds := m.settings(ctx)
	rows, err := m.s.db.Maps(ctx, `SELECT id, host, coalesce(device_id,'') AS device_id, coalesce(vs_conn,'') AS vs_conn, host_ref FROM tapo_plugs`)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	credsChanged := creds != m.creds
	m.creds = creds
	next := map[string]*tapoPlug{}
	for _, r := range rows {
		id, host := r["id"].(string), r["host"].(string)
		p := m.plugs[id]
		if p == nil || p.host != host || credsChanged {
			if p != nil {
				p.client.Close()
			}
			p = &tapoPlug{id: id, host: host, client: tapo.New(host, creds), live: TapoLive{Vendor: "TP-Link", Error: "not read yet"}}
		}
		p.target = plugTarget{device: r["device_id"].(string), conn: r["vs_conn"].(string), host: r["host_ref"].(string)}
		next[id] = p
	}
	for id, p := range m.plugs {
		if next[id] == nil {
			p.client.Close()
		}
	}
	m.plugs = next
}

// poke makes the poller run now (after a change).
func (m *tapoMgr) poke() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *tapoMgr) pollAll(ctx context.Context) {
	m.mu.Lock()
	list := make([]*tapoPlug, 0, len(m.plugs))
	for _, p := range m.plugs {
		list = append(list, p)
	}
	configured := m.creds.Username != ""
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range list {
		wg.Add(1)
		go func(p *tapoPlug) {
			defer wg.Done()
			if !configured {
				m.setLive(p, TapoLive{Vendor: "TP-Link", Error: "enter the TP-Link account in Settings → Smart plugs"}, false)
				return
			}
			rctx, cancel := context.WithTimeout(ctx, 9*time.Second)
			st, err := p.client.Read(rctx)
			cancel()
			if err != nil {
				m.setLive(p, TapoLive{Vendor: "TP-Link", Error: err.Error()}, false)
				return
			}
			on := st.On
			m.setLive(p, TapoLive{Available: true, Vendor: "TP-Link", Product: st.Model, Firmware: st.Firmware, RSSI: st.RSSI, On: &on,
				Watts: st.Watts, Volts: st.Volts, Amps: st.Amps, TodayKWh: st.TodayKWh, MonthKWh: st.MonthKWh}, true)
		}(p)
	}
	wg.Wait()
}

func (m *tapoMgr) setLive(p *tapoPlug, l TapoLive, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if ok {
		p.seen = now
		l.At = now.UTC().Format(time.RFC3339)
	} else if !p.seen.IsZero() && now.Sub(p.seen) < 3*tapoPoll {
		// one missed poll on Wi-Fi: keep the last reading, note the error
		keep := p.live
		keep.Error = l.Error
		l = keep
	}
	p.live = l
}

// watts finds the reading of a measuring plug assigned to a device or to a vSphere host.
func (m *tapoMgr) watts(device, conn, host string) (float64, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plugs {
		t := p.target
		if (device != "" && t.device == device) || (conn != "" && t.conn == conn && t.host == host) {
			if p.live.Available && p.live.Watts != nil {
				return *p.live.Watts, p.live.Product, true
			}
		}
	}
	return 0, "", false
}

func (m *tapoMgr) live(id string) (TapoLive, *tapo.Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.plugs[id]
	if p == nil {
		return TapoLive{}, nil, false
	}
	return p.live, p.client, true
}

// ---- API ----

// GET /api/tapo — the account (without the password) and every plug. ?device=<id> filters.
func (s *Server) getTapo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, _ := s.tp.settings(ctx)
	rows, err := s.db.Maps(ctx, `SELECT p.id, p.name, p.host, p.device_id, p.vs_conn, p.host_ref, p.created_at,
		coalesce(nullif(d.display_name,''), d.hostname) AS device_name, c.name AS conn_name
		FROM tapo_plugs p LEFT JOIN devices d ON d.id=p.device_id LEFT JOIN vsphere_connections c ON c.id=p.vs_conn ORDER BY p.created_at`)
	if err != nil {
		fail(w, err)
		return
	}
	device := r.URL.Query().Get("device")
	hostNames := map[string]string{}
	plugs := []map[string]any{}
	for _, row := range rows {
		if device != "" && row["device_id"] != device {
			continue
		}
		if conn, _ := row["vs_conn"].(string); conn != "" {
			if _, ok := hostNames[conn]; !ok {
				if inv, _, _ := s.loadInventory(ctx, conn); inv != nil {
					for _, h := range inv.Hosts {
						hostNames[conn+"/"+h.Ref] = h.Name
					}
				}
				hostNames[conn] = "loaded"
			}
			row["host_name"] = hostNames[conn+"/"+row["host_ref"].(string)]
		}
		if l, _, ok := s.tp.live(row["id"].(string)); ok {
			row["plug"] = l
		}
		plugs = append(plugs, row)
	}
	writeJSON(w, 200, map[string]any{"configured": st.Username != "" && st.Password != "", "username": st.Username, "plugs": plugs})
}

// PUT /api/tapo/settings {username, password} — an empty password keeps the saved one.
func (s *Server) putTapoSettings(w http.ResponseWriter, r *http.Request) {
	var in TapoSettings
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	in.Username = strings.TrimSpace(in.Username)
	old, _ := s.tp.settings(ctx)
	switch {
	case in.Username == "":
		in.Password = "" // clearing the account
	case in.Password == "":
		if old.Password == "" {
			fail(w, badRequest("enter the TP-Link account password"))
			return
		}
		in.Password = old.Password
	default:
		in.Password = s.box.seal(in.Password)
	}
	if err := s.db.SetSetting(ctx, "tapo", in); err != nil {
		fail(w, err)
		return
	}
	s.tp.reload(ctx)
	s.tp.poke()
	s.audit(ctx, userFrom(r), r, "tapo.settings", "settings", "tapo", map[string]any{"username": in.Username})
	s.getTapo(w, r)
}

// validTapoHost accepts an IP address or host name, optionally with a port.
func validTapoHost(h string) bool {
	if h == "" || strings.ContainsAny(h, "/?#@ ") {
		return false
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return h != ""
}

// POST /api/tapo/plugs {host, name} — read the plug once with the saved account, then keep it.
func (s *Server) addTapoPlug(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host string `json:"host"`
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	in.Host = strings.TrimSpace(in.Host)
	if !validTapoHost(in.Host) {
		fail(w, badRequest("enter the plug's IP address, e.g. 10.0.0.50 (the Tapo app shows it under the plug's Device Info)"))
		return
	}
	st, creds := s.tp.settings(ctx)
	if st.Username == "" || creds.Password == "" {
		fail(w, badRequest("save the TP-Link account first"))
		return
	}
	var dup bool
	_ = s.db.QueryRow(ctx, `SELECT true FROM tapo_plugs WHERE host=$1`, in.Host).Scan(&dup)
	if dup {
		fail(w, badRequest("that plug is already added"))
		return
	}
	c := tapo.New(in.Host, creds)
	defer c.Close()
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := c.Read(rctx)
	if err != nil {
		fail(w, badRequest(tapoMessage(err)))
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = firstNonEmpty(strings.TrimSpace(info.Nickname), "Tapo "+info.Model)
	}
	var id string
	if err := s.db.QueryRow(ctx, `INSERT INTO tapo_plugs (name, host) VALUES ($1,$2) RETURNING id`, name, in.Host).Scan(&id); err != nil {
		fail(w, err)
		return
	}
	s.tp.reload(ctx)
	s.tp.poke()
	s.audit(ctx, userFrom(r), r, "tapo.add", "tapo_plug", id, map[string]any{"name": name, "host": in.Host, "model": info.Model})
	writeJSON(w, 200, map[string]any{"id": id, "name": name, "model": info.Model, "energy": info.Watts != nil})
}

func tapoMessage(err error) string {
	var de *tapo.DeviceError
	switch {
	case errors.Is(err, tapo.ErrAuth), errors.Is(err, tapo.ErrRefused):
		return err.Error()
	case errors.As(err, &de):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the plug didn't answer in time: check the IP address and that the Apex server can reach it"
	}
	return "couldn't read the plug: " + err.Error()
}

// PATCH /api/tapo/plugs/{id} {name?, host?, device_id?, vs_conn?, host_ref?}
func (s *Server) updateTapoPlug(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Name    *string `json:"name"`
		Host    *string `json:"host"`
		Device  *string `json:"device_id"`
		Conn    *string `json:"vs_conn"`
		HostRef *string `json:"host_ref"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	var exists bool
	_ = s.db.QueryRow(ctx, `SELECT true FROM tapo_plugs WHERE id=$1`, id).Scan(&exists)
	if !exists {
		fail(w, ErrNotFound)
		return
	}
	change := map[string]any{}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			fail(w, badRequest("give the plug a name"))
			return
		}
		_, _ = s.db.Exec(ctx, `UPDATE tapo_plugs SET name=$2 WHERE id=$1`, id, n)
		change["name"] = n
	}
	if in.Host != nil {
		h := strings.TrimSpace(*in.Host)
		if !validTapoHost(h) {
			fail(w, badRequest("enter the plug's IP address"))
			return
		}
		if _, err := s.db.Exec(ctx, `UPDATE tapo_plugs SET host=$2 WHERE id=$1`, id, h); err != nil {
			fail(w, badRequest("another plug already has that address"))
			return
		}
		change["host"] = h
	}
	switch {
	case in.Device != nil && *in.Device != "":
		var ok bool
		_ = s.db.QueryRow(ctx, `SELECT true FROM devices WHERE id=$1`, *in.Device).Scan(&ok)
		if !ok {
			fail(w, badRequest("unknown device"))
			return
		}
		_, _ = s.db.Exec(ctx, `UPDATE tapo_plugs SET device_id=NULL WHERE device_id=$1 AND id<>$2`, *in.Device, id)
		_, _ = s.db.Exec(ctx, `UPDATE tapo_plugs SET device_id=$2, vs_conn=NULL, host_ref='' WHERE id=$1`, id, *in.Device)
		change["device_id"] = *in.Device
	case in.Conn != nil && *in.Conn != "":
		ref := ""
		if in.HostRef != nil {
			ref = *in.HostRef
		}
		if ref == "" {
			fail(w, badRequest("choose the host"))
			return
		}
		var ok bool
		_ = s.db.QueryRow(ctx, `SELECT true FROM vsphere_connections WHERE id=$1`, *in.Conn).Scan(&ok)
		if !ok {
			fail(w, badRequest("unknown vSphere connection"))
			return
		}
		_, _ = s.db.Exec(ctx, `UPDATE tapo_plugs SET vs_conn=NULL, host_ref='' WHERE vs_conn=$1 AND host_ref=$2 AND id<>$3`, *in.Conn, ref, id)
		_, _ = s.db.Exec(ctx, `UPDATE tapo_plugs SET device_id=NULL, vs_conn=$2, host_ref=$3 WHERE id=$1`, id, *in.Conn, ref)
		change["vs_conn"], change["host_ref"] = *in.Conn, ref
	case in.Device != nil || in.Conn != nil:
		_, _ = s.db.Exec(ctx, `UPDATE tapo_plugs SET device_id=NULL, vs_conn=NULL, host_ref='' WHERE id=$1`, id)
		change["unassigned"] = true
	}
	s.tp.reload(ctx)
	if in.Host != nil {
		s.tp.poke()
	}
	if len(change) > 0 {
		s.audit(ctx, userFrom(r), r, "tapo.update", "tapo_plug", id, change)
	}
	s.getTapo(w, r)
}

// POST /api/tapo/plugs/{id}/power {action: on|off|cycle, seconds?}
func (s *Server) tapoPlugPower(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Action  string `json:"action"`
		Seconds int    `json:"seconds"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	_, c, ok := s.tp.live(id)
	if !ok {
		fail(w, ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	switch in.Action {
	case "on", "off":
		if err := c.SetOn(ctx, in.Action == "on"); err != nil {
			fail(w, badRequest(tapoMessage(err)))
			return
		}
	case "cycle":
		secs := in.Seconds
		if secs <= 0 {
			secs = 10
		}
		if secs > 300 {
			secs = 300
		}
		if err := c.SetOn(ctx, false); err != nil {
			fail(w, badRequest(tapoMessage(err)))
			return
		}
		go func() {
			time.Sleep(time.Duration(secs) * time.Second)
			for i := 0; i < 5; i++ {
				bctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				err := c.SetOn(bctx, true)
				cancel()
				if err == nil {
					s.tp.poke()
					return
				}
				time.Sleep(5 * time.Second)
			}
		}()
		in.Seconds = secs
	default:
		fail(w, badRequest("action must be on, off or cycle"))
		return
	}
	s.tp.poke()
	s.audit(r.Context(), userFrom(r), r, "tapo.power", "tapo_plug", id, map[string]any{"action": in.Action, "seconds": in.Seconds})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// DELETE /api/tapo/plugs/{id}
func (s *Server) deleteTapoPlug(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	tag, err := s.db.Exec(ctx, `DELETE FROM tapo_plugs WHERE id=$1`, id)
	if err != nil {
		fail(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, ErrNotFound)
		return
	}
	s.tp.reload(ctx)
	s.audit(ctx, userFrom(r), r, "tapo.remove", "tapo_plug", id, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}
