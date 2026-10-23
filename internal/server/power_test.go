package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPowerPage(t *testing.T) {
	e := newITEnv(t)
	ctx := context.Background()
	var clientA, clientB string
	_ = e.s.db.QueryRow(ctx, `INSERT INTO clients (name) VALUES ('Power A '||gen_random_uuid()) RETURNING id`).Scan(&clientA)
	_ = e.s.db.QueryRow(ctx, `INSERT INTO clients (name) VALUES ('Power B '||gen_random_uuid()) RETURNING id`).Scan(&clientB)
	t.Cleanup(func() {
		_, _ = e.s.db.Exec(ctx, `DELETE FROM clients WHERE id IN ($1,$2)`, clientA, clientB)
	})
	loc := e.s.energySettings(ctx).Loc
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	mk := func(name, client string, w float64) string {
		var id string
		if err := e.s.db.QueryRow(ctx, `INSERT INTO devices (hostname, secret_hash, client_id, online, power_w, power_kind, power_src, power_at)
			VALUES ($1,'x',$2,true,$3,'measured','plug',now()) RETURNING id`, name, client, w).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	big := mk("big-server", clientA, 300)
	small := mk("small-pc", clientB, 40)
	for _, r := range []struct {
		id, day string
		wh, est float64
	}{{big, yesterday, 7200, 0}, {big, today, 3000, 0}, {small, today, 500, 400}} {
		if _, err := e.s.db.Exec(ctx, `INSERT INTO device_energy_daily (device_id, day, wh, est_wh) VALUES ($1,$2::date,$3,$4)`, r.id, r.day, r.wh, r.est); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		ts := time.Now().Add(-time.Duration(i) * 5 * time.Minute)
		_, _ = e.s.db.Exec(ctx, `INSERT INTO device_metrics (device_id, ts, cpu, mem, disk, power_w) VALUES ($1,$3,1,1,1,300),($2,$3,1,1,1,40)`, big, small, ts)
	}

	q := "?from=" + yesterday + "&to=" + today
	c, m, raw := e.do("viewer", "GET", "/api/power"+q+"&client="+clientA, nil)
	if c != 200 {
		t.Fatalf("power: %d %s", c, raw)
	}
	src := m["sources"].([]any)
	if len(src) != 1 || src[0].(map[string]any)["name"] != "big-server" || m["kwh"].(float64) != 10.2 {
		t.Fatalf("client A: %s", raw)
	}
	c, m, raw = e.do("viewer", "GET", "/api/power"+q, nil)
	if c != 200 {
		t.Fatalf("power: %s", raw)
	}
	var names []string
	for _, s := range m["sources"].([]any) {
		names = append(names, s.(map[string]any)["name"].(string))
	}
	if len(names) < 2 || names[0] != "big-server" {
		t.Fatalf("order: %v", names)
	}
	days := m["days"].([]any)
	if len(days) != 2 {
		t.Fatalf("days: %v", days)
	}
	if c, _, _ := e.do("viewer", "GET", "/api/power?from=2026-02-01&to=2026-01-01", nil); c != 400 {
		t.Fatal("reversed range accepted")
	}

	c, m, raw = e.do("viewer", "GET", "/api/power/history"+q+"&keys=d:"+big, nil)
	if c != 200 {
		t.Fatalf("history: %s", raw)
	}
	pts := m["points"].([]any)
	if len(pts) == 0 {
		t.Fatalf("no points: %s", raw)
	}
	last := pts[len(pts)-1].(map[string]any)
	if last["d:"+big] == nil || last["total"].(float64) < 300 {
		t.Fatalf("point: %v", last)
	}

	req := httptest.NewRequest("GET", "/api/power/report.pdf"+q, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.cookies["viewer"]})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF")) || rec.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf: %d %q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
}
