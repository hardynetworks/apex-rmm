package server

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// GET /api/power/report.pdf?from=&to=&client= — the Power page as a printable report.
func (s *Server) powerReportPDF(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr, err := parsePowerRange(r, s.energySettings(ctx).Loc)
	if err != nil {
		fail(w, err)
		return
	}
	rep, err := s.buildPowerReport(ctx, pr)
	if err != nil {
		fail(w, err)
		return
	}
	company := s.conf().CompanyName
	if company == "" {
		company = "Apex RMM"
	}
	b, err := renderPowerPDF(rep, company, time.Now().In(pr.From.Location()))
	if err != nil {
		fail(w, err)
		return
	}
	name := "power-report-" + rep.From + "-to-" + rep.To
	if rep.Client != "" {
		name += "-" + slug(rep.Client)
	}
	s.audit(ctx, userFrom(r), r, "power.report", "power", "", map[string]any{"from": rep.From, "to": rep.To, "client": rep.Client})
	w.Header().Set("Content-Type", "application/pdf")
	disp := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s.pdf"`, disp, name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

var typeLabel = map[string]string{"server": "Server", "desktop": "Desktop", "laptop": "Laptop", "virtual": "Virtual machine", "other": "Other"}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func fmtKWhPDF(k float64) string {
	switch {
	case k < 1:
		return fmt.Sprintf("%.0f Wh", k*1000)
	case k >= 100:
		return commas(math.Round(k), 0) + " kWh"
	case k >= 10:
		return fmt.Sprintf("%.1f kWh", k)
	}
	return fmt.Sprintf("%.2f kWh", k)
}

func fmtWPDF(v float64) string {
	if v >= 10000 {
		return fmt.Sprintf("%.1f kW", v/1000)
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f W", v)
	}
	return fmt.Sprintf("%.1f W", v)
}

func fmtMoney(cur string, v float64) string {
	if v >= 1000 {
		return cur + commas(math.Round(v), 0)
	}
	return cur + fmt.Sprintf("%.2f", v)
}

// commas formats n with thousands separators.
func commas(n float64, dp int) string {
	s := fmt.Sprintf("%.*f", dp, n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

func fmtRange(from, to string) string {
	f, _ := time.Parse("2006-01-02", from)
	t, _ := time.Parse("2006-01-02", to)
	if from == to {
		return f.Format("January 2, 2006")
	}
	if f.Year() == t.Year() {
		if f.Month() == t.Month() && f.Day() == 1 && t.AddDate(0, 0, 1).Day() == 1 {
			return f.Format("January 2006")
		}
		return f.Format("Jan 2") + " - " + t.Format("Jan 2, 2006")
	}
	return f.Format("Jan 2, 2006") + " - " + t.Format("Jan 2, 2006")
}

func renderPowerPDF(rep *powerReport, company string, now time.Time) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "Letter", "")
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	cur := tr(rep.Currency)
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 18)
	pdf.SetTitle("Power report "+fmtRange(rep.From, rep.To), true)
	pdf.SetAuthor(company, true)
	pdf.SetCreator("Apex RMM", true)
	pageW, _ := pdf.GetPageSize()
	cw := pageW - 30 // content width

	ink := func() { pdf.SetTextColor(28, 32, 40) }
	muted := func() { pdf.SetTextColor(110, 116, 130) }
	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("Helvetica", "", 8)
		muted()
		pdf.CellFormat(cw/2, 5, tr(company+" - Power report, "+fmtRange(rep.From, rep.To)), "", 0, "L", false, 0, "")
		pdf.CellFormat(cw/2, 5, fmt.Sprintf("Page %d of {nb}", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AliasNbPages("{nb}")
	pdf.AddPage()

	// header
	pdf.SetFillColor(8, 145, 178)
	pdf.Rect(15, 15, 3, 18, "F")
	pdf.SetXY(21, 14.5)
	pdf.SetFont("Helvetica", "B", 19)
	ink()
	pdf.CellFormat(cw-6, 9, "Power report", "", 1, "L", false, 0, "")
	pdf.SetX(21)
	pdf.SetFont("Helvetica", "", 11)
	scope := "All clients"
	if rep.Client != "" {
		scope = rep.Client
	}
	pdf.CellFormat(cw-6, 6, tr(fmtRange(rep.From, rep.To)+"  |  "+scope), "", 1, "L", false, 0, "")
	pdf.SetX(21)
	pdf.SetFont("Helvetica", "", 8.5)
	muted()
	pdf.CellFormat(cw-6, 5, tr(fmt.Sprintf("%s  |  Generated %s (%s)", company, now.Format("Jan 2, 2006 3:04 PM"), rep.TZ)), "", 1, "L", false, 0, "")
	pdf.Ln(5)

	// summary tiles
	type tile struct{ label, value, sub string }
	tiles := []tile{
		{"Energy used", fmtKWhPDF(rep.KWh), ""},
		{"Cost", "-", "set a price per kWh in Settings"},
		{"Average draw", fmtWPDF(rep.AvgW), "over the period"},
		{"Sources", fmt.Sprintf("%d", rep.Devices+rep.Hosts), fmt.Sprintf("%d devices, %d ESXi hosts", rep.Devices, rep.Hosts)},
	}
	if rep.Cost != nil {
		tiles[1] = tile{"Cost", fmtMoney(cur, *rep.Cost), fmt.Sprintf("at %s%.4g per kWh", cur, rep.Rate)}
	}
	if rep.EstKWh > 0 && rep.KWh > 0 {
		tiles[0].sub = fmt.Sprintf("%.0f%% estimated", rep.EstKWh/rep.KWh*100)
	} else {
		tiles[0].sub = "all measured"
	}
	tw, gap := (cw-9)/4, 3.0
	y0 := pdf.GetY()
	for i, t := range tiles {
		x := 15 + float64(i)*(tw+gap)
		pdf.SetFillColor(244, 246, 249)
		pdf.SetDrawColor(226, 230, 236)
		pdf.RoundedRect(x, y0, tw, 22, 2, "1234", "FD")
		pdf.SetXY(x+4, y0+3)
		pdf.SetFont("Helvetica", "", 8)
		muted()
		pdf.CellFormat(tw-8, 4, strings.ToUpper(t.label), "", 2, "L", false, 0, "")
		pdf.SetFont("Helvetica", "B", 15)
		ink()
		pdf.CellFormat(tw-8, 8, t.value, "", 2, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 7.5)
		muted()
		pdf.CellFormat(tw-8, 4, tr(t.sub), "", 2, "L", false, 0, "")
	}
	pdf.SetY(y0 + 28)

	// daily energy chart: devices and ESXi hosts stacked
	section := func(title string) {
		pdf.SetFont("Helvetica", "B", 11.5)
		ink()
		pdf.CellFormat(cw, 7, title, "", 1, "L", false, 0, "")
	}
	section("Energy per day")
	chartH := 52.0
	cx, cy := 15+14.0, pdf.GetY()+7
	cwid := cw - 14
	maxV := 0.0
	type split struct{ dev, host float64 }
	sp := make([]split, len(rep.Days))
	for i, d := range rep.Days {
		for k, v := range d.By {
			if strings.HasPrefix(k, "h:") {
				sp[i].host += v
			} else {
				sp[i].dev += v
			}
		}
		maxV = math.Max(maxV, d.KWh)
	}
	nice := niceCeil(maxV)
	pdf.SetFont("Helvetica", "", 7)
	pdf.SetDrawColor(226, 230, 236)
	pdf.SetLineWidth(0.2)
	for i := 0; i <= 4; i++ {
		v := nice * float64(i) / 4
		yy := cy + chartH - chartH*float64(i)/4
		pdf.Line(cx, yy, cx+cwid, yy)
		muted()
		pdf.SetXY(15, yy-2)
		pdf.CellFormat(13, 4, trimZero(v), "", 0, "R", false, 0, "")
	}
	pdf.SetXY(15, cy-6.5)
	pdf.CellFormat(13, 4, "kWh", "", 0, "R", false, 0, "")
	if n := len(rep.Days); n > 0 && nice > 0 {
		slot := cwid / float64(n)
		bw := math.Min(slot*0.72, 9)
		every := int(math.Ceil(float64(n) / math.Max(2, math.Floor(cwid/16))))
		for i, d := range rep.Days {
			x := cx + slot*float64(i) + (slot-bw)/2
			hDev := chartH * sp[i].dev / nice
			hHost := chartH * sp[i].host / nice
			base := cy + chartH
			if hDev > 0 {
				pdf.SetFillColor(42, 120, 214) // devices: categorical slot 1
				pdf.Rect(x, base-hDev, bw, hDev, "F")
			}
			if hHost > 0 {
				pdf.SetFillColor(235, 104, 52) // ESXi hosts: slot 2
				gapH := 0.0
				if hDev > 0 {
					gapH = 0.35
				}
				pdf.Rect(x, base-hDev-hHost, bw, math.Max(0, hHost-gapH), "F")
			}
			if i%every == 0 {
				t, _ := time.Parse("2006-01-02", d.Day)
				muted()
				pdf.SetXY(x+bw/2-8, base+1)
				pdf.CellFormat(16, 4, t.Format("Jan 2"), "", 0, "C", false, 0, "")
			}
		}
	}
	// legend
	ly := cy + chartH + 7
	lx := cx
	for _, l := range []struct {
		name    string
		r, g, b int
	}{{"Devices (agents)", 42, 120, 214}, {"ESXi hosts", 235, 104, 52}} {
		pdf.SetFillColor(l.r, l.g, l.b)
		pdf.Rect(lx, ly+0.8, 3, 3, "F")
		pdf.SetXY(lx+4, ly)
		ink()
		pdf.SetFont("Helvetica", "", 8)
		wd := pdf.GetStringWidth(l.name) + 2
		pdf.CellFormat(wd, 4.6, l.name, "", 0, "L", false, 0, "")
		lx += wd + 8
	}
	pdf.SetY(ly + 9)

	// per-source table
	section("Usage by device and host")
	showClient := rep.Client == ""
	cols := []struct {
		title string
		w     float64
		align string
	}{{"#", 8, "R"}, {"Name", 0, "L"}, {"Type", 26, "L"}, {"Client", 30, "L"}, {"Avg draw", 20, "R"}, {"Energy", 23, "R"}, {"Share", 15, "R"}, {"Cost", 20, "R"}}
	if !showClient {
		cols = append(cols[:3], cols[4:]...)
	}
	fixed := 0.0
	for _, c := range cols {
		fixed += c.w
	}
	for i := range cols {
		if cols[i].w == 0 {
			cols[i].w = cw - fixed
		}
	}
	head := func() {
		pdf.SetFont("Helvetica", "B", 8)
		pdf.SetFillColor(236, 239, 244)
		muted()
		for _, c := range cols {
			pdf.CellFormat(c.w, 6.5, c.title, "", 0, c.align, true, 0, "")
		}
		pdf.Ln(-1)
	}
	head()
	pdf.SetFont("Helvetica", "", 8.5)
	anyEst := false
	fit := func(s string, w float64) string {
		s = tr(s)
		for pdf.GetStringWidth(s) > w-2 && len(s) > 1 {
			s = s[:len(s)-1]
		}
		return s
	}
	listed, idle := rep.Sources, 0
	for len(listed) > 0 && listed[len(listed)-1].KWh == 0 {
		listed = listed[:len(listed)-1]
		idle++
	}
	for i, p := range listed {
		if pdf.GetY() > 255 {
			pdf.AddPage()
			head()
			pdf.SetFont("Helvetica", "", 8.5)
		}
		fill := i%2 == 1
		pdf.SetFillColor(248, 249, 251)
		ink()
		typ := p.Group
		if l, ok := typeLabel[typ]; ok && p.Type == "device" {
			typ = l
		}
		client := p.Client
		if client == "" {
			client = "-"
		}
		name := p.Name
		if p.EstKWh > p.KWh*0.5 {
			name += " *"
			anyEst = true
		}
		cost := "-"
		if p.Cost != nil {
			cost = fmtMoney(cur, *p.Cost)
		}
		vals := []string{fmt.Sprint(i + 1), name, typ, client, fmtWPDF(p.AvgW), fmtKWhPDF(p.KWh), fmt.Sprintf("%.1f%%", p.SharePct), cost}
		if !showClient {
			vals = append(vals[:3], vals[4:]...)
		}
		for j, c := range cols {
			pdf.CellFormat(c.w, 6, fit(vals[j], c.w), "", 0, c.align, fill, 0, "")
		}
		pdf.Ln(-1)
	}
	if len(rep.Sources) == 0 {
		muted()
		pdf.CellFormat(cw, 8, "No device or ESXi host reported power in this period.", "", 1, "C", false, 0, "")
	} else {
		// total row
		pdf.SetDrawColor(200, 205, 214)
		pdf.Line(15, pdf.GetY(), 15+cw, pdf.GetY())
		pdf.SetFont("Helvetica", "B", 8.5)
		ink()
		cost := "-"
		if rep.Cost != nil {
			cost = fmtMoney(cur, *rep.Cost)
		}
		vals := []string{"", "Total", "", "", fmtWPDF(rep.AvgW), fmtKWhPDF(rep.KWh), "100%", cost}
		if !showClient {
			vals = append(vals[:3], vals[4:]...)
		}
		for j, c := range cols {
			pdf.CellFormat(c.w, 7, vals[j], "", 0, c.align, false, 0, "")
		}
		pdf.Ln(8)
		if idle > 0 {
			pdf.SetFont("Helvetica", "", 8)
			muted()
			pdf.CellFormat(cw, 5, fmt.Sprintf("%d more device(s) reported power but used no energy in this period (offline or not yet measured).", idle), "", 1, "L", false, 0, "")
			pdf.Ln(2)
		}
	}

	// notes
	pdf.SetFont("Helvetica", "", 7.5)
	muted()
	notes := []string{
		"Energy is counted from each source's power readings while it was online; time a device was offline or not reporting is not included.",
		"ESXi host figures come from vCenter (or a smart plug assigned to the host) and include the virtual machines running on them.",
	}
	if anyEst {
		notes = append(notes, "* Mostly estimated from CPU load rather than measured. Assign a smart plug or set idle/full-load watts for closer figures.")
	}
	if rep.Rate > 0 {
		notes = append(notes, fmt.Sprintf("Costs use %s%.4g per kWh (Settings > General > Power & energy). Days follow the %s time zone.", cur, rep.Rate, rep.TZ))
	} else {
		notes = append(notes, fmt.Sprintf("No electricity price is set, so costs are not shown. Days follow the %s time zone.", rep.TZ))
	}
	for _, n := range notes {
		pdf.MultiCell(cw, 3.8, tr(n), "", "L", false)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

func trimZero(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}
