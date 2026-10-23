package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The Power page: every device and ESXi host that reports power, for a date range (optionally one
// client): energy and cost per source, daily kWh per source, and power over time. The same report
// is also produced as a PDF (power_pdf.go).

// powerRange is a date range in the energy time zone, from and to inclusive.
type powerRange struct {
	From, To   time.Time // midnight of the first day, midnight after the last day
	FromS, ToS string    // YYYY-MM-DD (inclusive)
	Client     string    // "" = all clients
	ClientName string
}

func parsePowerRange(r *http.Request, loc *time.Location) (powerRange, error) {
	q := r.URL.Query()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	pr := powerRange{From: today.AddDate(0, 0, -29), To: today.AddDate(0, 0, 1), Client: strings.TrimSpace(q.Get("client"))}
	if v := q.Get("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return pr, badRequest("from must be a date like 2026-10-01")
		}
		pr.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return pr, badRequest("to must be a date like 2026-10-31")
		}
		pr.To = t.AddDate(0, 0, 1)
	}
	if !pr.To.After(pr.From) {
		return pr, badRequest("the end date must be on or after the start date")
	}
	if pr.To.Sub(pr.From) > 367*24*time.Hour {
		return pr, badRequest("pick a range of at most a year")
	}
	pr.FromS, pr.ToS = pr.From.Format("2006-01-02"), pr.To.AddDate(0, 0, -1).Format("2006-01-02")
	return pr, nil
}

func (pr powerRange) client() any {
	if pr.Client == "" {
		return nil
	}
	return pr.Client
}

// hours is how long the range covers, up to now (for average draw).
func (pr powerRange) hours() float64 {
	end := pr.To
	if n := time.Now(); n.Before(end) {
		end = n
	}
	h := end.Sub(pr.From).Hours()
	if h < 0 {
		return 0
	}
	return h
}

// powerSource is one device or ESXi host on the Power page.
type powerSource struct {
	Key      string   `json:"key"`  // d:<device id> or h:<connection id>|<host ref>
	Type     string   `json:"type"` // device | host
	ID       string   `json:"id"`   // device id, or connection id for hosts
	Ref      string   `json:"ref,omitempty"`
	Name     string   `json:"name"`
	Client   string   `json:"client"`
	Group    string   `json:"group"` // device type, or "ESXi host"
	Kind     string   `json:"kind"`  // measured | partial | estimate
	Src      string   `json:"src"`
	Online   bool     `json:"online"`
	NowW     *float64 `json:"now_w"`
	KWh      float64  `json:"kwh"`
	EstKWh   float64  `json:"est_kwh"`
	AvgW     float64  `json:"avg_w"`
	Cost     *float64 `json:"cost"`
	SharePct float64  `json:"share_pct"`
}

type powerDay struct {
	Day string             `json:"day"`
	KWh float64            `json:"kwh"`
	Est float64            `json:"est_kwh"`
	By  map[string]float64 `json:"by"`
}

type powerReport struct {
	Range    powerRange    `json:"-"`
	From     string        `json:"from"`
	To       string        `json:"to"`
	Client   string        `json:"client"`
	Sources  []powerSource `json:"sources"`
	Days     []powerDay    `json:"days"`
	KWh      float64       `json:"kwh"`
	EstKWh   float64       `json:"est_kwh"`
	Cost     *float64      `json:"cost"`
	AvgW     float64       `json:"avg_w"`
	NowW     float64       `json:"now_w"`
	Devices  int           `json:"devices"`
	Hosts    int           `json:"hosts"`
	Rate     float64       `json:"rate"`
	Currency string        `json:"currency"`
	TZ       string        `json:"tz"`
}

func (s *Server) buildPowerReport(ctx context.Context, pr powerRange) (*powerReport, error) {
	es := s.energySettings(ctx)
	cl := pr.client()
	if pr.Client != "" {
		if err := s.db.QueryRow(ctx, `SELECT name FROM clients WHERE id=$1`, pr.Client).Scan(&pr.ClientName); err != nil {
			return nil, badRequest("unknown client")
		}
	}
	rep := &powerReport{Range: pr, From: pr.FromS, To: pr.ToS, Client: pr.ClientName, Rate: es.Rate, Currency: es.Currency, TZ: es.Loc.String()}
	byKey := map[string]*powerSource{}
	add := func(ps powerSource) *powerSource {
		if p := byKey[ps.Key]; p != nil {
			return p
		}
		p := ps
		byKey[p.Key] = &p
		return &p
	}

	// devices: everything that has reported power at some point
	devs, err := s.db.Maps(ctx, `SELECT d.id, coalesce(nullif(d.display_name,''), d.hostname) AS name, coalesce(c.name,'') AS client,
		`+deviceTypeExpr+` AS grp, d.power_kind, d.power_src, d.online,
		CASE WHEN d.online AND d.power_at > now() - interval '5 minutes' THEN d.power_w::float8 END AS now_w,
		(coalesce(sum(e.wh),0)/1000)::float8 AS kwh, (coalesce(sum(e.est_wh),0)/1000)::float8 AS est_kwh
		FROM devices d LEFT JOIN clients c ON c.id=d.client_id
		LEFT JOIN device_energy_daily e ON e.device_id=d.id AND e.day BETWEEN $1::date AND $2::date
		WHERE d.power_at IS NOT NULL AND ($3::text IS NULL OR d.client_id=$3)
		GROUP BY d.id, c.name`, pr.FromS, pr.ToS, cl)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		ps := powerSource{Key: "d:" + str(d["id"]), Type: "device", ID: str(d["id"]), Name: str(d["name"]), Client: str(d["client"]),
			Group: str(d["grp"]), Kind: str(d["power_kind"]), Src: str(d["power_src"]), Online: d["online"] == true,
			KWh: num(d["kwh"]), EstKWh: num(d["est_kwh"])}
		if v, ok := d["now_w"].(float64); ok {
			ps.NowW = &v
		}
		add(ps)
	}

	// ESXi hosts: current inventory plus any with energy in the range
	cur, err := s.db.Maps(ctx, `SELECT c.id AS conn, coalesce(cl.name,'') AS client, h->>'ref' AS ref, h->>'name' AS name,
		(h->>'power_watts')::float8 AS power_w, coalesce(h->>'power_source','') AS src,
		i.updated_at > now() - make_interval(secs => greatest(300, c.poll_seconds*3)) AND c.enabled AS fresh
		FROM vsphere_inventory i JOIN vsphere_connections c ON c.id=i.connection_id LEFT JOIN clients cl ON cl.id=c.client_id,
		jsonb_array_elements(i.data->'hosts') h
		WHERE ($1::text IS NULL OR c.client_id=$1)`, cl)
	if err != nil {
		return nil, err
	}
	for _, h := range cur {
		if h["power_w"] == nil {
			continue
		}
		ps := powerSource{Key: "h:" + str(h["conn"]) + "|" + str(h["ref"]), Type: "host", ID: str(h["conn"]), Ref: str(h["ref"]),
			Name: str(h["name"]), Client: str(h["client"]), Group: "ESXi host", Kind: "measured", Src: "vsphere"}
		if h["src"] == "estimate" {
			ps.Kind = "estimate"
		}
		if h["fresh"] == true {
			v := num(h["power_w"])
			ps.NowW, ps.Online = &v, true
		}
		add(ps)
	}
	hostE, err := s.db.Maps(ctx, `SELECT e.connection_id AS conn, e.host_ref AS ref, max(e.host_name) AS name, coalesce(max(cl.name),'') AS client,
		(sum(e.wh)/1000)::float8 AS kwh, (sum(e.est_wh)/1000)::float8 AS est_kwh
		FROM vsphere_host_energy_daily e JOIN vsphere_connections c ON c.id=e.connection_id LEFT JOIN clients cl ON cl.id=c.client_id
		WHERE e.day BETWEEN $1::date AND $2::date AND ($3::text IS NULL OR c.client_id=$3) GROUP BY 1, 2`, pr.FromS, pr.ToS, cl)
	if err != nil {
		return nil, err
	}
	for _, h := range hostE {
		p := add(powerSource{Key: "h:" + str(h["conn"]) + "|" + str(h["ref"]), Type: "host", ID: str(h["conn"]), Ref: str(h["ref"]),
			Name: str(h["name"]), Client: str(h["client"]), Group: "ESXi host", Kind: "measured", Src: "vsphere"})
		p.KWh, p.EstKWh = num(h["kwh"]), num(h["est_kwh"])
	}

	// daily kWh per source
	days := map[string]*powerDay{}
	addDay := func(day, key string, kwh, est float64) {
		d := days[day]
		if d == nil {
			d = &powerDay{Day: day, By: map[string]float64{}}
			days[day] = d
		}
		d.KWh += kwh
		d.Est += est
		d.By[key] += kwh
	}
	dd, err := s.db.Maps(ctx, `SELECT to_char(e.day,'YYYY-MM-DD') AS day, e.device_id AS id, (e.wh/1000)::float8 AS kwh, (e.est_wh/1000)::float8 AS est
		FROM device_energy_daily e JOIN devices d ON d.id=e.device_id
		WHERE e.day BETWEEN $1::date AND $2::date AND ($3::text IS NULL OR d.client_id=$3)`, pr.FromS, pr.ToS, cl)
	if err != nil {
		return nil, err
	}
	for _, r := range dd {
		addDay(str(r["day"]), "d:"+str(r["id"]), num(r["kwh"]), num(r["est"]))
	}
	hd, err := s.db.Maps(ctx, `SELECT to_char(e.day,'YYYY-MM-DD') AS day, e.connection_id AS conn, e.host_ref AS ref, (e.wh/1000)::float8 AS kwh, (e.est_wh/1000)::float8 AS est
		FROM vsphere_host_energy_daily e JOIN vsphere_connections c ON c.id=e.connection_id
		WHERE e.day BETWEEN $1::date AND $2::date AND ($3::text IS NULL OR c.client_id=$3)`, pr.FromS, pr.ToS, cl)
	if err != nil {
		return nil, err
	}
	for _, r := range hd {
		addDay(str(r["day"]), "h:"+str(r["conn"])+"|"+str(r["ref"]), num(r["kwh"]), num(r["est"]))
	}
	// every day of the range, so the chart has gaps where nothing was recorded
	last := pr.To
	if n := time.Now().In(pr.From.Location()); n.Before(last) {
		last = n
	}
	for t := pr.From; t.Before(last); t = t.AddDate(0, 0, 1) {
		day := t.Format("2006-01-02")
		if days[day] == nil {
			days[day] = &powerDay{Day: day, By: map[string]float64{}}
		}
	}
	for _, d := range days {
		rep.Days = append(rep.Days, *d)
	}
	sort.Slice(rep.Days, func(i, j int) bool { return rep.Days[i].Day < rep.Days[j].Day })

	hours := pr.hours()
	for _, p := range byKey {
		rep.KWh += p.KWh
		rep.EstKWh += p.EstKWh
		if p.NowW != nil {
			rep.NowW += *p.NowW
		}
		if p.Type == "host" {
			rep.Hosts++
		} else {
			rep.Devices++
		}
		if hours > 0 {
			p.AvgW = p.KWh * 1000 / hours
		}
		if es.Rate > 0 {
			c := p.KWh * es.Rate
			p.Cost = &c
		}
		rep.Sources = append(rep.Sources, *p)
	}
	for i := range rep.Sources {
		if rep.KWh > 0 {
			rep.Sources[i].SharePct = rep.Sources[i].KWh / rep.KWh * 100
		}
	}
	sort.Slice(rep.Sources, func(i, j int) bool {
		a, b := rep.Sources[i], rep.Sources[j]
		if a.KWh != b.KWh {
			return a.KWh > b.KWh
		}
		if (a.NowW == nil) != (b.NowW == nil) {
			return a.NowW != nil
		}
		if a.NowW != nil && *a.NowW != *b.NowW {
			return *a.NowW > *b.NowW
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	if rep.Sources == nil {
		rep.Sources = []powerSource{}
	}
	if hours > 0 {
		rep.AvgW = rep.KWh * 1000 / hours
	}
	if es.Rate > 0 {
		c := rep.KWh * es.Rate
		rep.Cost = &c
	}
	return rep, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// GET /api/power?from=&to=&client=
func (s *Server) powerOverview(w http.ResponseWriter, r *http.Request) {
	pr, err := parsePowerRange(r, s.energySettings(r.Context()).Loc)
	if err != nil {
		fail(w, err)
		return
	}
	rep, err := s.buildPowerReport(r.Context(), pr)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

// bucketFor picks a bucket size giving about 240 points over span.
func bucketFor(span time.Duration) time.Duration {
	for _, b := range []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
		time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour} {
		if span/b <= 300 {
			return b
		}
	}
	return 24 * time.Hour
}

// GET /api/power/history?from=&to=&client=&keys=k1,k2 — average watts per time bucket for the
// listed sources; everything else is summed into "other".
func (s *Server) powerHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr, err := parsePowerRange(r, s.energySettings(ctx).Loc)
	if err != nil {
		fail(w, err)
		return
	}
	keep := map[string]bool{}
	for _, k := range strings.Split(r.URL.Query().Get("keys"), ",") {
		if k = strings.TrimSpace(k); k != "" && len(keep) < 12 {
			keep[k] = true
		}
	}
	end := pr.To
	if n := time.Now(); n.Before(end) {
		end = n
	}
	// raw readings are kept for a limited time; say from when the chart has data
	devKeep := time.Duration(s.conf().MetricsRetentionDays) * 24 * time.Hour
	start := pr.From
	b := bucketFor(end.Sub(start))
	secs := int(b / time.Second)
	cl := pr.client()
	type pt = map[string]float64
	buckets := map[int64]pt{}
	put := func(t time.Time, key string, wv float64) {
		k := t.Unix()
		p := buckets[k]
		if p == nil {
			p = pt{}
			buckets[k] = p
		}
		if !keep[key] {
			key = "other"
		}
		p[key] += wv
		p["total"] += wv
	}
	rows, err := s.db.Query(ctx, `SELECT to_timestamp(floor(extract(epoch FROM m.ts) / $3) * $3) AS t, m.device_id, avg(m.power_w)::float8
		FROM device_metrics m JOIN devices d ON d.id=m.device_id
		WHERE m.ts >= $1 AND m.ts < $2 AND m.power_w IS NOT NULL AND ($4::text IS NULL OR d.client_id=$4)
		GROUP BY 1, 2`, start, end, secs, cl)
	if err != nil {
		fail(w, err)
		return
	}
	for rows.Next() {
		var t time.Time
		var id string
		var v float64
		if err := rows.Scan(&t, &id, &v); err != nil {
			rows.Close()
			fail(w, err)
			return
		}
		put(t, "d:"+id, v)
	}
	rows.Close()
	rows, err = s.db.Query(ctx, `SELECT to_timestamp(floor(extract(epoch FROM p.ts) / $3) * $3) AS t, p.connection_id, p.host_ref, avg(p.watts)::float8
		FROM vsphere_host_power p JOIN vsphere_connections c ON c.id=p.connection_id
		WHERE p.ts >= $1 AND p.ts < $2 AND p.watts IS NOT NULL AND ($4::text IS NULL OR c.client_id=$4)
		GROUP BY 1, 2, 3`, start, end, secs, cl)
	if err != nil {
		fail(w, err)
		return
	}
	for rows.Next() {
		var t time.Time
		var conn, ref string
		var v float64
		if err := rows.Scan(&t, &conn, &ref, &v); err != nil {
			rows.Close()
			fail(w, err)
			return
		}
		put(t, "h:"+conn+"|"+ref, v)
	}
	rows.Close()
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		row := map[string]any{"ts": time.Unix(k, 0).UTC().Format(time.RFC3339)}
		for kk, v := range buckets[k] {
			row[kk] = v
		}
		out = append(out, row)
	}
	writeJSON(w, 200, map[string]any{
		"points":         out,
		"bucket_seconds": secs,
		// device readings older than this aren't kept (hosts keep 30 days)
		"device_history_from": time.Now().Add(-devKeep).UTC().Format(time.RFC3339),
		"host_history_from":   time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339),
	})
}
