package agent

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// Parsers for platform tools' output, kept here so they're tested on every platform.

// parseDCMI reads "<completion>|<bytes>" where the bytes start (after an optional completion
// code) with the DCMI group id 0xDC followed by the current power, little-endian.
func parseDCMI(out string) (float64, bool) {
	cc, data, ok := strings.Cut(strings.TrimSpace(out), "|")
	if !ok || strings.TrimSpace(cc) != "0" {
		return 0, false
	}
	var b []int
	for _, f := range strings.Split(data, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return 0, false
		}
		b = append(b, v)
	}
	for i := 0; i < len(b) && i < 2; i++ {
		if b[i] == 0xDC && i+2 < len(b) {
			w := float64(b[i+1] | b[i+2]<<8)
			return w, w > 0
		}
	}
	return 0, false
}

type batteryInfo struct {
	present   bool
	external  bool
	systemIn  float64 // W from the adapter, 0 if unknown
	discharge float64 // W from the battery, 0 if not discharging
}

var (
	reSystemPowerIn = regexp.MustCompile(`"SystemPowerIn"\s*=\s*(\d+)`)
	reInstantAmp    = regexp.MustCompile(`"InstantAmperage"\s*=\s*(-?\d+)`)
	reAmperage      = regexp.MustCompile(`"Amperage"\s*=\s*(-?\d+)`)
	reVoltage       = regexp.MustCompile(`"Voltage"\s*=\s*(\d+)`)
	reExternal      = regexp.MustCompile(`"ExternalConnected"\s*=\s*(Yes|No)`)
)

func parseIOReg(out string, ok bool) batteryInfo {
	var b batteryInfo
	if !ok || !strings.Contains(out, "AppleSmartBattery") {
		return b
	}
	b.present = true
	if m := reExternal.FindStringSubmatch(out); m != nil {
		b.external = m[1] == "Yes"
	}
	if m := reSystemPowerIn.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			b.systemIn = v / 1000 // mW
		}
	}
	if !b.external {
		amp := reInstantAmp.FindStringSubmatch(out)
		if amp == nil {
			amp = reAmperage.FindStringSubmatch(out)
		}
		volt := reVoltage.FindStringSubmatch(out)
		if amp != nil && volt != nil {
			// ioreg prints negative currents as unsigned 64-bit numbers
			a, err := strconv.ParseInt(amp[1], 10, 64)
			if err != nil {
				if u, err2 := strconv.ParseUint(amp[1], 10, 64); err2 == nil {
					a = int64(u)
				}
			}
			v, _ := strconv.ParseFloat(volt[1], 64)
			if a < 0 {
				b.discharge = float64(-a) / 1000 * v / 1000
			}
		}
	}
	return b
}

var (
	rePMLine  = regexp.MustCompile(`(?m)^(CPU|GPU|ANE) Power:\s*([\d.]+)\s*mW`)
	rePMIntel = regexp.MustCompile(`(?m)package power \(CPUs\+GT\+SA\):\s*([\d.]+)\s*W`)
)

func parsePowermetrics(out string) []proto.PowerPart {
	var parts []proto.PowerPart
	names := map[string]string{"CPU": "CPU", "GPU": "GPU", "ANE": "Neural Engine"}
	for _, m := range rePMLine.FindAllStringSubmatch(out, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		parts = append(parts, proto.PowerPart{Name: names[m[1]], Watts: v / 1000, Source: "powermetrics"})
	}
	if m := rePMIntel.FindStringSubmatch(out); m != nil && len(parts) == 0 {
		v, _ := strconv.ParseFloat(m[1], 64)
		parts = append(parts, proto.PowerPart{Name: "CPU package", Watts: v, Source: "powermetrics"})
	}
	return parts
}
