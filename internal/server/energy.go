package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // energy days follow the configured time zone, even in a minimal container

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// Power monitoring: agents report watts with every metrics sample; the server keeps the latest
// reading on the device, the history in device_metrics, and adds each sample's energy
// (watts × time since the previous sample) to a per-device daily total in kWh.

// maxSampleGap is the longest stretch one sample may account for: a longer silence means the
// agent was off or offline, and that time isn't counted.
const maxSampleGap = 2 * time.Minute

type energySettings struct {
	Rate     float64 // price per kWh (0 = not set)
	Currency string
	Loc      *time.Location
}

var (
	locMu    sync.Mutex
	locCache = map[string]*time.Location{}
)

func loadLoc(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	locMu.Lock()
	defer locMu.Unlock()
	if l, ok := locCache[name]; ok {
		return l
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		l = time.Local
	}
	locCache[name] = l
	return l
}

func (s *Server) energySettings(ctx context.Context) energySettings {
	var st Settings
	s.db.Setting(ctx, "general", &st)
	cur := st.Currency
	if cur == "" {
		cur = "$"
	}
	return energySettings{Rate: st.EnergyRate, Currency: cur, Loc: loadLoc(st.EnergyTZ)}
}

func (s *Server) recordEnergy(ctx context.Context, id string, e *proto.Energy) {
	if e == nil {
		_, _ = s.db.Exec(ctx, `UPDATE devices SET power_w=NULL, power_kind='', power_src='', power_info='{}', ups_on_battery=false WHERE id=$1`, id)
		return
	}
	info, _ := json.Marshal(map[string]any{"parts": e.Parts, "ups": e.UPS, "note": e.Note})
	onBatt := e.UPS != nil && e.UPS.OnBattery
	var prev *time.Time
	err := s.db.QueryRow(ctx, `UPDATE devices d SET power_w=$2, power_kind=$3, power_src=$4, power_at=now(), power_info=$5, ups_on_battery=$6
		FROM (SELECT power_at AS old FROM devices WHERE id=$1) o WHERE d.id=$1 RETURNING o.old`,
		id, e.Watts, e.Kind, e.Source, string(info), onBatt).Scan(&prev)
	if err != nil {
		slog.Error("record power", "device", id, "err", err)
		return
	}
	dt := 30 * time.Second // first reading: one metrics interval
	if prev != nil {
		dt = time.Since(*prev)
		if dt > maxSampleGap || dt < 0 {
			dt = 30 * time.Second
		}
	}
	wh := e.Watts * dt.Hours()
	if wh <= 0 {
		return
	}
	est := 0.0
	if e.Kind != "measured" {
		est = wh
	}
	day := time.Now().In(s.energySettings(ctx).Loc).Format("2006-01-02")
	_, err = s.db.Exec(ctx, `INSERT INTO device_energy_daily (device_id, day, wh, est_wh) VALUES ($1,$2::date,$3,$4)
		ON CONFLICT (device_id, day) DO UPDATE SET wh = device_energy_daily.wh + EXCLUDED.wh, est_wh = device_energy_daily.est_wh + EXCLUDED.est_wh`,
		id, day, wh, est)
	if err != nil {
		slog.Error("record energy", "device", id, "err", err)
	}
}

// periods returns today, the first of this month and the start of a days-long window, as dates
// in the energy time zone.
func periods(loc *time.Location, days int) (today, month, from string) {
	now := time.Now().In(loc)
	today = now.Format("2006-01-02")
	month = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).Format("2006-01-02")
	from = now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	return
}

func queryDays(r *http.Request) int {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 366 {
		days = 30
	}
	return days
}

// GET /api/devices/{id}/energy?days=30
func (s *Server) deviceEnergy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	es := s.energySettings(ctx)
	days := queryDays(r)
	today, month, from := periods(es.Loc, days)
	rows, err := s.db.Maps(ctx, `SELECT to_char(day,'YYYY-MM-DD') AS day, (wh/1000)::float8 AS kwh, (est_wh/1000)::float8 AS est_kwh
		FROM device_energy_daily WHERE device_id=$1 AND day >= $2::date ORDER BY day`, id, from)
	if err != nil {
		fail(w, err)
		return
	}
	tot, err := s.db.Map(ctx, `SELECT
		coalesce(sum(wh) FILTER (WHERE day = $2::date), 0)/1000 AS today,
		coalesce(sum(wh) FILTER (WHERE day >= $3::date), 0)/1000 AS month,
		coalesce(sum(wh) FILTER (WHERE day >= $4::date), 0)/1000 AS period,
		coalesce(sum(est_wh) FILTER (WHERE day >= $3::date), 0)/1000 AS month_est
		FROM device_energy_daily WHERE device_id=$1`, id, today, month, from)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"days": rows, "today_kwh": tot["today"], "month_kwh": tot["month"], "period_kwh": tot["period"],
		"month_est_kwh": tot["month_est"], "rate": es.Rate, "currency": es.Currency})
}

// GET /api/energy?days=30 — the whole fleet (optionally one client).
func (s *Server) fleetEnergy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	es := s.energySettings(ctx)
	days := queryDays(r)
	today, month, from := periods(es.Loc, days)
	var client any
	if c := r.URL.Query().Get("client"); c != "" {
		client = c
	}
	now, err := s.db.Map(ctx, `SELECT coalesce(sum(power_w),0)::float8 AS watts, count(*) AS devices,
		count(*) FILTER (WHERE power_kind='measured') AS measured, count(*) FILTER (WHERE power_kind='partial') AS partial,
		count(*) FILTER (WHERE power_kind='estimate') AS estimated
		FROM devices WHERE online AND power_w IS NOT NULL AND power_at > now() - interval '5 minutes' AND ($1::text IS NULL OR client_id=$1)`, client)
	if err != nil {
		fail(w, err)
		return
	}
	daysRows, err := s.db.Maps(ctx, `SELECT to_char(e.day,'YYYY-MM-DD') AS day, (sum(e.wh)/1000)::float8 AS kwh, (sum(e.est_wh)/1000)::float8 AS est_kwh
		FROM device_energy_daily e JOIN devices d ON d.id=e.device_id
		WHERE e.day >= $1::date AND ($2::text IS NULL OR d.client_id=$2) GROUP BY e.day ORDER BY e.day`, from, client)
	if err != nil {
		fail(w, err)
		return
	}
	tot, err := s.db.Map(ctx, `SELECT
		(coalesce(sum(e.wh) FILTER (WHERE e.day = $1::date), 0)/1000)::float8 AS today,
		(coalesce(sum(e.wh) FILTER (WHERE e.day >= $2::date), 0)/1000)::float8 AS month,
		(coalesce(sum(e.wh) FILTER (WHERE e.day >= $3::date), 0)/1000)::float8 AS period
		FROM device_energy_daily e JOIN devices d ON d.id=e.device_id WHERE e.day >= least($2::date, $3::date) AND ($4::text IS NULL OR d.client_id=$4)`,
		today, month, from, client)
	if err != nil {
		fail(w, err)
		return
	}
	top, err := s.db.Maps(ctx, `SELECT d.id, 'device' AS type, coalesce(nullif(d.display_name,''), d.hostname) AS name, d.online,
		CASE WHEN d.online AND d.power_at > now() - interval '5 minutes' THEN d.power_w END AS power_w, d.power_kind, d.power_src,
		(coalesce(sum(e.wh),0)/1000)::float8 AS month_kwh
		FROM devices d LEFT JOIN device_energy_daily e ON e.device_id=d.id AND e.day >= $1::date
		WHERE ($2::text IS NULL OR d.client_id=$2) AND (d.power_at IS NOT NULL)
		GROUP BY d.id ORDER BY month_kwh DESC, power_w DESC NULLS LAST LIMIT 10`, month, client)
	if err != nil {
		fail(w, err)
		return
	}
	ups, err := s.db.Maps(ctx, `SELECT d.id, coalesce(nullif(d.display_name,''), d.hostname) AS device, d.power_info->'ups' AS ups
		FROM devices d WHERE d.online AND d.power_at > now() - interval '5 minutes' AND jsonb_typeof(d.power_info->'ups') = 'object'
		AND ($1::text IS NULL OR d.client_id=$1) ORDER BY d.ups_on_battery DESC, 2`, client)
	if err != nil {
		fail(w, err)
		return
	}
	// vSphere hosts count like measured devices (their draw includes the VMs on them).
	hostsNow, err := s.db.Map(ctx, `SELECT coalesce(sum((h->>'power_watts')::float8),0)::float8 AS watts, count(*) AS hosts,
		count(*) FILTER (WHERE h->>'power_source' = 'estimate') AS estimated
		FROM vsphere_inventory i JOIN vsphere_connections c ON c.id=i.connection_id, jsonb_array_elements(i.data->'hosts') h
		WHERE c.enabled AND i.updated_at > now() - make_interval(secs => greatest(300, c.poll_seconds*3))
		AND h ? 'power_watts' AND ($1::text IS NULL OR c.client_id=$1)`, client)
	if err == nil {
		now["watts"] = num(now["watts"]) + num(hostsNow["watts"])
		now["hosts"] = hostsNow["hosts"]
		now["hosts_estimated"] = hostsNow["estimated"]
	}
	hostDays, _ := s.db.Maps(ctx, `SELECT to_char(e.day,'YYYY-MM-DD') AS day, (sum(e.wh)/1000)::float8 AS kwh, (sum(e.est_wh)/1000)::float8 AS est_kwh
		FROM vsphere_host_energy_daily e JOIN vsphere_connections c ON c.id=e.connection_id
		WHERE e.day >= $1::date AND ($2::text IS NULL OR c.client_id=$2) GROUP BY e.day`, from, client)
	daysRows = mergeDays(daysRows, hostDays)
	if ht, err := s.db.Map(ctx, `SELECT
		(coalesce(sum(e.wh) FILTER (WHERE e.day = $1::date), 0)/1000)::float8 AS today,
		(coalesce(sum(e.wh) FILTER (WHERE e.day >= $2::date), 0)/1000)::float8 AS month,
		(coalesce(sum(e.wh) FILTER (WHERE e.day >= $3::date), 0)/1000)::float8 AS period
		FROM vsphere_host_energy_daily e JOIN vsphere_connections c ON c.id=e.connection_id
		WHERE e.day >= least($2::date, $3::date) AND ($4::text IS NULL OR c.client_id=$4)`, today, month, from, client); err == nil {
		for _, k := range []string{"today", "month", "period"} {
			tot[k] = num(tot[k]) + num(ht[k])
		}
	}
	hostTop, _ := s.db.Maps(ctx, `WITH cur AS (
			SELECT c.id AS conn, h->>'ref' AS ref, h->>'name' AS name, (h->>'power_watts')::float8 AS power_w, h->>'power_source' AS src
			FROM vsphere_inventory i JOIN vsphere_connections c ON c.id=i.connection_id, jsonb_array_elements(i.data->'hosts') h
			WHERE c.enabled AND i.updated_at > now() - make_interval(secs => greatest(300, c.poll_seconds*3)) AND ($2::text IS NULL OR c.client_id=$2)),
		mon AS (
			SELECT e.connection_id AS conn, e.host_ref AS ref, max(e.host_name) AS name, (sum(e.wh)/1000)::float8 AS month_kwh
			FROM vsphere_host_energy_daily e JOIN vsphere_connections c ON c.id=e.connection_id
			WHERE e.day >= $1::date AND ($2::text IS NULL OR c.client_id=$2) GROUP BY 1, 2)
		SELECT coalesce(cur.name, mon.name) AS name, cur.power_w, cur.src, coalesce(mon.month_kwh, 0)::float8 AS month_kwh,
			coalesce(cur.conn, mon.conn) AS connection_id, coalesce(cur.ref, mon.ref) AS host_ref
		FROM cur FULL JOIN mon ON mon.conn=cur.conn AND mon.ref=cur.ref
		WHERE cur.power_w IS NOT NULL OR mon.month_kwh > 0`, month, client)
	for _, h := range hostTop {
		h["type"] = "vsphere"
		h["online"] = h["power_w"] != nil
		h["power_kind"], h["power_src"] = "measured", "vsphere"
		if h["src"] == "estimate" {
			h["power_kind"] = "estimate"
		}
		delete(h, "src")
		top = append(top, h)
	}
	sort.SliceStable(top, func(i, j int) bool {
		a, b := num(top[i]["month_kwh"]), num(top[j]["month_kwh"])
		if a != b {
			return a > b
		}
		return num(top[i]["power_w"]) > num(top[j]["power_w"])
	})
	if len(top) > 10 {
		top = top[:10]
	}
	writeJSON(w, 200, map[string]any{"now": now, "days": daysRows, "today_kwh": tot["today"], "month_kwh": tot["month"],
		"period_kwh": tot["period"], "top": top, "ups": ups, "rate": es.Rate, "currency": es.Currency})
}

// num reads a number from a database row value (nil is 0).
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// mergeDays adds the hosts' daily kWh into the devices' days (by date, sorted).
func mergeDays(days, hosts []map[string]any) []map[string]any {
	if len(hosts) == 0 {
		return days
	}
	by := map[string]map[string]any{}
	for _, d := range days {
		by[d["day"].(string)] = d
	}
	for _, h := range hosts {
		day, _ := h["day"].(string)
		if d, ok := by[day]; ok {
			d["kwh"] = num(d["kwh"]) + num(h["kwh"])
			d["est_kwh"] = num(d["est_kwh"]) + num(h["est_kwh"])
		} else {
			row := map[string]any{"day": day, "kwh": num(h["kwh"]), "est_kwh": num(h["est_kwh"])}
			by[day] = row
			days = append(days, row)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i]["day"].(string) < days[j]["day"].(string) })
	return days
}

// ---- per-device power settings ----

func cleanEnergyConf(c *proto.EnergyConfig) error {
	switch c.Mode {
	case "", "auto":
		c.Mode = "auto"
	case "plug", "ups", "estimate", "off":
	default:
		return errors.New("power source must be auto, plug, ups, estimate or off")
	}
	c.PlugAddr, c.PlugField, c.UPSAddr, c.UPSName = strings.TrimSpace(c.PlugAddr), strings.TrimSpace(c.PlugField), strings.TrimSpace(c.UPSAddr), strings.TrimSpace(c.UPSName)
	if c.Mode == "plug" {
		switch c.PlugType {
		case "shelly", "tasmota", "kasa", "json":
		default:
			return errors.New("plug type must be shelly, tasmota, kasa or json")
		}
		if c.PlugAddr == "" {
			return errors.New("enter the smart plug's address")
		}
	}
	if c.PlugType != "json" && strings.Contains(c.PlugAddr, "/") && !strings.HasPrefix(c.PlugAddr, "http") {
		return errors.New("enter the plug's IP address or host name")
	}
	if c.UPSAddr != "" {
		host := c.UPSAddr
		if h, _, err := net.SplitHostPort(c.UPSAddr); err == nil {
			host = h
		}
		if strings.ContainsAny(host, "/ ") {
			return errors.New("enter the UPS server as host or host:port")
		}
	}
	for _, v := range []*float64{&c.IdleW, &c.MaxW, &c.ExtraW} {
		if *v < 0 || *v > 100000 {
			return errors.New("watts must be between 0 and 100000")
		}
	}
	if c.UPSShare < 0 || c.UPSShare > 100 {
		return errors.New("the UPS share must be between 0 and 100 %")
	}
	return nil
}

func (s *Server) deviceEnergyConf(ctx context.Context, id string) proto.EnergyConfig {
	var raw []byte
	var c proto.EnergyConfig
	if s.db.QueryRow(ctx, `SELECT energy_conf FROM devices WHERE id=$1`, id).Scan(&raw) == nil {
		_ = json.Unmarshal(raw, &c)
	}
	if c.Mode == "" {
		c.Mode = "auto"
	}
	return c
}

// pushEnergyConfig sends the device's power settings to its agent (on connect and on change).
func (s *Server) pushEnergyConfig(id string) {
	c := s.deviceEnergyConf(context.Background(), id)
	_ = s.hub.Send(id, proto.TypeEnergyConfig, c)
}

func mustJSONObj(v any) string {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return "{}"
	}
	return string(b)
}
