package server

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/vsphere"
)

// ESXi host energy page: each host's live draw and what it costs.
//
// vSphere refreshes host power every 20 seconds (the real-time counter interval; IPMI sensors are
// no faster), so while someone has the page open the hosts' power is read at that rate, which is
// much more often than the inventory poll. The page ticks every second between readings.

// liveEvery is how often hosts are re-read while the energy page is open.
const liveEvery = 20 * time.Second

type liveHosts struct {
	mu       sync.Mutex
	at       time.Time
	hosts    []vsphere.Host
	err      string
	fetching bool
}

var liveCache sync.Map // connection id -> *liveHosts

// livePower returns the latest power reading of a connection's hosts, reading vSphere again when
// it is older than liveEvery. Only the first request waits; later ones get the cached reading
// while a fresh one is fetched in the background.
func (m *vsphereMgr) livePower(ctx context.Context, c *vsConn) *liveHosts {
	v, _ := liveCache.LoadOrStore(c.ID, &liveHosts{})
	lh := v.(*liveHosts)
	lh.mu.Lock()
	fresh := time.Since(lh.at) < liveEvery
	first := lh.at.IsZero() && lh.err == ""
	start := !fresh && !lh.fetching
	if start {
		lh.fetching = true
	}
	lh.mu.Unlock()
	if start {
		fetch := func(ctx context.Context) {
			hosts, err := m.readHostPower(ctx, c)
			lh.mu.Lock()
			lh.fetching = false
			if err != nil {
				lh.err = err.Error()
				if lh.at.IsZero() {
					lh.at = time.Now().Add(-liveEvery + 5*time.Second) // retry in a few seconds, not on every request
				}
			} else {
				lh.at, lh.hosts, lh.err = time.Now(), hosts, ""
			}
			lh.mu.Unlock()
		}
		if first {
			fetch(ctx)
		} else {
			go fetch(context.Background())
		}
	}
	return lh
}

func (m *vsphereMgr) readHostPower(ctx context.Context, c *vsConn) ([]vsphere.Host, error) {
	conn, err := m.session(ctx, c)
	if err != nil {
		return nil, err
	}
	hosts, err := conn.HostPower(ctx)
	if err != nil {
		m.dropSessionOnly(c.ID)
		return nil, err
	}
	// The readings also go into the history and the daily kWh totals.
	m.s.recordHostPower(ctx, c.ID, &vsphere.Inventory{Hosts: hosts}, int(liveEvery/time.Second))
	return hosts, nil
}

// GET /api/vsphere/connections/{id}/energy
func (s *Server) vsphereEnergy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	c, err := s.vsLoad(ctx, id)
	if err != nil {
		fail(w, ErrNotFound)
		return
	}
	es := s.energySettings(ctx)

	// live reading, else the last stored inventory
	var hosts []vsphere.Host
	var at time.Time
	live, liveErr := false, ""
	if c.Enabled {
		lh := s.vs.livePower(ctx, c)
		lh.mu.Lock()
		if !lh.at.IsZero() && lh.hosts != nil {
			hosts, at, live = lh.hosts, lh.at, true
		}
		liveErr = lh.err
		lh.mu.Unlock()
	}
	if !live {
		inv, invAt, err := s.loadInventory(ctx, id)
		if err != nil {
			fail(w, err)
			return
		}
		if inv != nil {
			hosts, at = inv.Hosts, invAt
		}
	}

	now := time.Now().In(es.Loc)
	today := now.Format("2006-01-02")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, es.Loc)
	nextMonth := monthStart.AddDate(0, 1, 0)
	yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, es.Loc)
	rows, err := s.db.Maps(ctx, `SELECT host_ref,
		(coalesce(sum(wh) FILTER (WHERE day = $2::date), 0)/1000)::float8 AS today_kwh,
		(coalesce(sum(wh) FILTER (WHERE day >= $3::date), 0)/1000)::float8 AS month_kwh,
		(coalesce(sum(wh), 0)/1000)::float8 AS year_kwh,
		(coalesce(sum(est_wh) FILTER (WHERE day >= $3::date), 0)/1000)::float8 AS month_est_kwh
		FROM vsphere_host_energy_daily WHERE connection_id=$1 AND day >= $4::date GROUP BY host_ref`,
		id, today, monthStart.Format("2006-01-02"), yearStart.Format("2006-01-02"))
	if err != nil {
		fail(w, err)
		return
	}
	byRef := map[string]map[string]any{}
	for _, r := range rows {
		byRef[r["host_ref"].(string)] = r
	}

	type hostOut struct {
		Ref         string   `json:"ref"`
		Name        string   `json:"name"`
		Model       string   `json:"model"`
		State       string   `json:"state"`
		Watts       *float64 `json:"watts"`
		Source      string   `json:"source"`
		Note        string   `json:"note,omitempty"`
		CPUPct      float64  `json:"cpu_pct"`
		TodayKWh    float64  `json:"today_kwh"`
		MonthKWh    float64  `json:"month_kwh"`
		MonthEstKWh float64  `json:"month_est_kwh"`
		YearKWh     float64  `json:"year_kwh"`
	}
	out := make([]hostOut, 0, len(hosts))
	for _, h := range hosts {
		ho := hostOut{Ref: h.Ref, Name: h.Name, Model: (h.Vendor + " " + h.Model), State: h.ConnectionState,
			Watts: h.PowerWatts, Source: h.PowerSource, Note: h.PowerNote, CPUPct: h.CPUPct()}
		if e := byRef[h.Ref]; e != nil {
			ho.TodayKWh, ho.MonthKWh, ho.YearKWh, ho.MonthEstKWh = num(e["today_kwh"]), num(e["month_kwh"]), num(e["year_kwh"]), num(e["month_est_kwh"])
		}
		out = append(out, ho)
	}
	next := liveEvery - time.Since(at)
	if !live || next < time.Second {
		next = time.Second * 5
	}
	writeJSON(w, 200, map[string]any{
		"connection": map[string]any{"id": c.ID, "name": c.Name},
		"hosts":      out,
		"at":         at,
		"live":       live,
		"live_error": liveErr,
		"next_ms":    next.Milliseconds(),
		"rate":       es.Rate,
		"currency":   es.Currency,
		// hours left in this month (for "projected this month") and in the day
		"month_hours_left": nextMonth.Sub(now).Hours(),
		"day_hours_left":   time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, es.Loc).Sub(now).Hours(),
		"month_days":       nextMonth.AddDate(0, 0, -1).Day(),
	})
}

// GET /api/vsphere/connections/{id}/power-history?minutes=60 — every host's raw readings, to seed
// the energy page's live chart.
func (s *Server) vsphereEnergyHistory(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if v, err := strconv.Atoi(r.URL.Query().Get("minutes")); err == nil && v >= 1 && v <= 24*60 {
		minutes = v
	}
	rows, err := s.db.Maps(r.Context(), `SELECT ts, host_ref, watts FROM vsphere_host_power
		WHERE connection_id=$1 AND watts IS NOT NULL AND ts > now() - make_interval(mins => $2) ORDER BY ts`, chi.URLParam(r, "id"), minutes)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
