package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/matter"
	"github.com/hardynetworks/apex-rmm/internal/proto"
	"github.com/hardynetworks/apex-rmm/internal/vsphere"
)

// Matter smart plugs. Apex talks to a Matter controller (matterjs-server, run next to Apex with
// "docker compose --profile matter up -d") over its WebSocket API. A plug can be assigned to an
// agent device or a vSphere host: its measured watts then replace that device's or host's power
// reading. Plugs without power measurement can still be switched (power cycling).

const defaultMatterURL = "ws://host.docker.internal:5580/ws"

// MatterSettings are stored under the "matter" setting.
type MatterSettings struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

type plugTarget struct {
	device string // device id
	conn   string // vSphere connection id
	host   string // host ref
}

type matterMgr struct {
	s      *Server
	mu     sync.Mutex
	ctx    context.Context
	client *matter.Client
	stop   context.CancelFunc
	key    string
	assign map[int64]plugTarget // node id -> what it powers
}

func newMatterMgr(s *Server) *matterMgr { return &matterMgr{s: s, assign: map[int64]plugTarget{}} }

// settings: the environment (APEX_MATTER_URL) overrides the dashboard.
func (m *matterMgr) settings(ctx context.Context) (MatterSettings, bool) {
	if u := strings.TrimSpace(os.Getenv("APEX_MATTER_URL")); u != "" {
		return MatterSettings{Enabled: true, URL: u}, true
	}
	var st MatterSettings
	m.s.db.Setting(ctx, "matter", &st)
	if st.URL == "" {
		st.URL = defaultMatterURL
	}
	return st, false
}

// start connects (when enabled) and keeps the client in step with the settings.
func (m *matterMgr) start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.loadAssignments(ctx)
	m.reload(ctx)
}

func (m *matterMgr) reload(ctx context.Context) {
	st, _ := m.settings(ctx)
	key := ""
	if st.Enabled && st.URL != "" {
		key = st.URL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == m.key && (key == "" || m.client != nil) {
		return
	}
	if m.stop != nil {
		m.stop()
		m.stop, m.client = nil, nil
	}
	m.key = key
	if key == "" || m.ctx == nil {
		return
	}
	c := matter.New(key)
	cctx, cancel := context.WithCancel(m.ctx)
	m.client, m.stop = c, cancel
	go c.Run(cctx)
}

func (m *matterMgr) get() *matter.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

func (m *matterMgr) loadAssignments(ctx context.Context) {
	rows, err := m.s.db.Maps(ctx, `SELECT node_id, coalesce(device_id,'') AS device_id, coalesce(vs_conn,'') AS vs_conn, host_ref FROM matter_plugs`)
	if err != nil {
		return
	}
	a := map[int64]plugTarget{}
	for _, r := range rows {
		a[int64(num(r["node_id"]))] = plugTarget{device: r["device_id"].(string), conn: r["vs_conn"].(string), host: r["host_ref"].(string)}
	}
	m.mu.Lock()
	m.assign = a
	m.mu.Unlock()
}

// plugFor finds the live plug assigned to a device, or to a vSphere host (conn, ref).
func (m *matterMgr) plugFor(device, conn, host string) (matter.Plug, bool) {
	m.mu.Lock()
	var id int64 = -1
	for nid, t := range m.assign {
		if (device != "" && t.device == device) || (conn != "" && t.conn == conn && t.host == host) {
			id = nid
			break
		}
	}
	c := m.client
	m.mu.Unlock()
	if id < 0 || c == nil {
		return matter.Plug{}, false
	}
	return c.Plug(id)
}

// devicePlugEnergy replaces an agent's power reading with its plug's, when the plug measures power.
func (m *matterMgr) devicePlugEnergy(deviceID string, e *proto.Energy) *proto.Energy {
	var out *proto.Energy
	if p, ok := m.plugFor(deviceID, "", ""); ok && p.Available && p.Watts != nil {
		out = &proto.Energy{Watts: *p.Watts, Source: "matter", Kind: "measured"}
	} else if w, model, ok := m.s.tp.watts(deviceID, "", ""); ok {
		_ = model
		out = &proto.Energy{Watts: w, Source: "tapo", Kind: "measured"}
	} else {
		return e
	}
	if e != nil {
		out.Parts, out.UPS = e.Parts, e.UPS
	}
	return out
}

// applyHostPlugs sets the power of vSphere hosts that have a measuring plug.
func (m *matterMgr) applyHostPlugs(conn string, hosts []vsphere.Host) {
	for i := range hosts {
		if p, ok := m.plugFor("", conn, hosts[i].Ref); ok && p.Available && p.Watts != nil {
			w := *p.Watts
			hosts[i].PowerWatts, hosts[i].PowerSource, hosts[i].PowerNote = &w, "plug", "Matter smart plug"
		} else if w, model, ok := m.s.tp.watts("", conn, hosts[i].Ref); ok {
			hosts[i].PowerWatts, hosts[i].PowerSource, hosts[i].PowerNote = &w, "plug", strings.TrimSpace("Tapo "+model)
		}
	}
}

// ---- API ----

func (s *Server) matterClient(w http.ResponseWriter) *matter.Client {
	c := s.mt.get()
	if c == nil {
		fail(w, badRequest("Matter is turned off: enable it in Settings → Smart plugs"))
		return nil
	}
	return c
}

// GET /api/matter — the controller connection and every plug (with what it is assigned to).
// ?device=<id> limits the plugs to the one assigned to that device.
func (s *Server) getMatter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, locked := s.mt.settings(ctx)
	out := map[string]any{"enabled": st.Enabled, "url": st.URL, "locked": locked, "status": matter.Status{State: "off"}, "plugs": []any{}}
	rows, err := s.db.Maps(ctx, `SELECT p.node_id, p.name, p.device_id, p.vs_conn, p.host_ref, p.created_at,
		coalesce(nullif(d.display_name,''), d.hostname) AS device_name, c.name AS conn_name
		FROM matter_plugs p LEFT JOIN devices d ON d.id=p.device_id LEFT JOIN vsphere_connections c ON c.id=p.vs_conn ORDER BY p.created_at`)
	if err != nil {
		fail(w, err)
		return
	}
	hostNames := map[string]string{}
	c := s.mt.get()
	if c != nil {
		out["status"] = c.Status()
	}
	device := r.URL.Query().Get("device")
	plugs := []map[string]any{}
	for _, row := range rows {
		id := int64(num(row["node_id"]))
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
		row["node_id"] = id
		if c != nil {
			if p, ok := c.Plug(id); ok {
				row["plug"] = p
			}
		}
		plugs = append(plugs, row)
	}
	out["plugs"] = plugs
	writeJSON(w, 200, out)
}

// PUT /api/matter/settings
func (s *Server) putMatterSettings(w http.ResponseWriter, r *http.Request) {
	var in MatterSettings
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		in.URL = defaultMatterURL
	}
	if !strings.HasPrefix(in.URL, "ws://") && !strings.HasPrefix(in.URL, "wss://") {
		fail(w, badRequest("the Matter server address starts with ws:// (for example "+defaultMatterURL+")"))
		return
	}
	if err := s.db.SetSetting(r.Context(), "matter", in); err != nil {
		fail(w, err)
		return
	}
	s.mt.reload(r.Context())
	s.audit(r.Context(), userFrom(r), r, "matter.settings", "settings", "matter", map[string]any{"enabled": in.Enabled, "url": in.URL})
	s.getMatter(w, r)
}

// POST /api/matter/plugs {code, name} — commission a plug with its pairing code.
func (s *Server) addMatterPlug(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	code := strings.TrimSpace(in.Code)
	digits := strings.NewReplacer("-", "", " ", "").Replace(code)
	if !(strings.HasPrefix(strings.ToUpper(code), "MT:") || (isDigits(digits) && (len(digits) == 11 || len(digits) == 21))) {
		fail(w, badRequest("enter the 11-digit pairing code (e.g. 3497-011-2332) or the MT:… code from the QR label"))
		return
	}
	c := s.matterClient(w)
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	p, err := c.Commission(ctx, code)
	if err != nil {
		if errors.Is(err, matter.ErrNotConnected) {
			fail(w, badRequest("Apex can't reach the Matter server; check Settings → Smart plugs"))
			return
		}
		fail(w, badRequest("pairing failed: "+err.Error()))
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimSpace(firstNonEmpty(p.Label, strings.TrimSpace(p.Vendor+" "+p.Product), "Smart plug"))
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO matter_plugs (node_id, name) VALUES ($1,$2) ON CONFLICT (node_id) DO UPDATE SET name=EXCLUDED.name`, p.NodeID, name); err != nil {
		fail(w, err)
		return
	}
	s.audit(ctx, userFrom(r), r, "matter.add", "matter_plug", strconv.FormatInt(p.NodeID, 10), map[string]any{"name": name, "vendor": p.Vendor, "product": p.Product})
	writeJSON(w, 200, map[string]any{"node_id": p.NodeID, "name": name, "plug": p})
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func nodeParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "node"), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrNotFound
	}
	return id, nil
}

// PATCH /api/matter/plugs/{node} {name?, device_id?, vs_conn?, host_ref?} — rename or assign.
// Assigning to a device clears the host and the other way round; empty strings unassign.
func (s *Server) updateMatterPlug(w http.ResponseWriter, r *http.Request) {
	id, err := nodeParam(r)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Name    *string `json:"name"`
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
	_ = s.db.QueryRow(ctx, `SELECT true FROM matter_plugs WHERE node_id=$1`, id).Scan(&exists)
	if !exists {
		fail(w, ErrNotFound)
		return
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			fail(w, badRequest("give the plug a name"))
			return
		}
		_, _ = s.db.Exec(ctx, `UPDATE matter_plugs SET name=$2 WHERE node_id=$1`, id, n)
	}
	assign := map[string]any{}
	switch {
	case in.Device != nil && *in.Device != "":
		var ok bool
		_ = s.db.QueryRow(ctx, `SELECT true FROM devices WHERE id=$1`, *in.Device).Scan(&ok)
		if !ok {
			fail(w, badRequest("unknown device"))
			return
		}
		// one plug per device / host: take it off any other plug first
		_, _ = s.db.Exec(ctx, `UPDATE matter_plugs SET device_id=NULL WHERE device_id=$1 AND node_id<>$2`, *in.Device, id)
		_, _ = s.db.Exec(ctx, `UPDATE matter_plugs SET device_id=$2, vs_conn=NULL, host_ref='' WHERE node_id=$1`, id, *in.Device)
		assign["device_id"] = *in.Device
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
		_, _ = s.db.Exec(ctx, `UPDATE matter_plugs SET vs_conn=NULL, host_ref='' WHERE vs_conn=$1 AND host_ref=$2 AND node_id<>$3`, *in.Conn, ref, id)
		_, _ = s.db.Exec(ctx, `UPDATE matter_plugs SET device_id=NULL, vs_conn=$2, host_ref=$3 WHERE node_id=$1`, id, *in.Conn, ref)
		assign["vs_conn"], assign["host_ref"] = *in.Conn, ref
	case in.Device != nil || in.Conn != nil: // empty: unassign
		_, _ = s.db.Exec(ctx, `UPDATE matter_plugs SET device_id=NULL, vs_conn=NULL, host_ref='' WHERE node_id=$1`, id)
		assign["unassigned"] = true
	}
	s.mt.loadAssignments(ctx)
	if len(assign) > 0 || in.Name != nil {
		if in.Name != nil {
			assign["name"] = *in.Name
		}
		s.audit(ctx, userFrom(r), r, "matter.update", "matter_plug", strconv.FormatInt(id, 10), assign)
	}
	s.getMatter(w, r)
}

// POST /api/matter/plugs/{node}/power {action: on|off|cycle, seconds?}
func (s *Server) matterPlugPower(w http.ResponseWriter, r *http.Request) {
	id, err := nodeParam(r)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Action  string `json:"action"`
		Seconds int    `json:"seconds"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	c := s.matterClient(w)
	if c == nil {
		return
	}
	if _, ok := c.Plug(id); !ok {
		fail(w, ErrNotFound)
		return
	}
	ctx := r.Context()
	switch in.Action {
	case "on", "off":
		if err := c.SetOn(ctx, id, in.Action == "on"); err != nil {
			fail(w, badRequest(err.Error()))
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
		if err := c.SetOn(ctx, id, false); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
		// switch back on in the background so the request returns straight away
		go func() {
			time.Sleep(time.Duration(secs) * time.Second)
			bctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for i := 0; i < 5; i++ {
				if err := c.SetOn(bctx, id, true); err == nil {
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
	s.audit(ctx, userFrom(r), r, "matter.power", "matter_plug", strconv.FormatInt(id, 10), map[string]any{"action": in.Action, "seconds": in.Seconds})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// DELETE /api/matter/plugs/{node} — remove the plug from the Matter fabric and from Apex.
func (s *Server) deleteMatterPlug(w http.ResponseWriter, r *http.Request) {
	id, err := nodeParam(r)
	if err != nil {
		fail(w, err)
		return
	}
	ctx := r.Context()
	if c := s.mt.get(); c != nil {
		if _, ok := c.Plug(id); ok {
			if err := c.Remove(ctx, id); err != nil && r.URL.Query().Get("force") != "1" {
				fail(w, badRequest("the Matter server couldn't remove the plug: "+err.Error()+" (remove it anyway with force)"))
				return
			}
		}
	}
	_, _ = s.db.Exec(ctx, `DELETE FROM matter_plugs WHERE node_id=$1`, id)
	s.mt.loadAssignments(ctx)
	s.audit(ctx, userFrom(r), r, "matter.remove", "matter_plug", strconv.FormatInt(id, 10), nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}
