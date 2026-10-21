//go:build darwin

package agent

import (
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// macOS power sources:
//   - the battery's power telemetry (MacBooks): "SystemPowerIn" is what the whole Mac draws from
//     the adapter; on battery, the discharge current × voltage
//   - powermetrics (root): CPU, GPU and Neural Engine power on Apple silicon, package power on Intel
type macPower struct {
	mu      sync.Mutex
	battAt  time.Time
	batt    batteryInfo
	pmAt    time.Time
	pmParts []proto.PowerPart
	pmFail  time.Time
}

func newPlatformPower() platformPower { return &macPower{} }

func (p *macPower) battery() batteryInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.battAt) < 10*time.Second {
		return p.batt
	}
	p.battAt = time.Now()
	out, err := runCmd(5*time.Second, "/usr/sbin/ioreg", "-rn", "AppleSmartBattery")
	p.batt = parseIOReg(out, err == nil)
	return p.batt
}

func (p *macPower) whole() (float64, string, bool) {
	b := p.battery()
	if b.external && b.systemIn > 0 {
		return b.systemIn, "telemetry", true
	}
	if !b.external && b.discharge > 0 {
		return b.discharge, "battery", true
	}
	return 0, "", false
}

func (p *macPower) onBattery() bool {
	b := p.battery()
	return b.present && !b.external
}

func hasBattery() bool {
	out, err := runCmd(5*time.Second, "/usr/sbin/ioreg", "-rn", "AppleSmartBattery")
	return err == nil && strings.Contains(out, "AppleSmartBattery")
}

// parts runs powermetrics for half a second (at most once every 25 s).
func (p *macPower) parts() []proto.PowerPart {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.pmAt) < 25*time.Second {
		return append([]proto.PowerPart(nil), p.pmParts...)
	}
	if time.Since(p.pmFail) < time.Hour {
		return nil
	}
	p.pmAt = time.Now()
	out, err := runCmd(10*time.Second, "/usr/bin/powermetrics", "-n", "1", "-i", "500", "--samplers", "cpu_power,gpu_power")
	if err != nil {
		p.pmFail, p.pmParts = time.Now(), nil
		return nil
	}
	p.pmParts = parsePowermetrics(out)
	return append([]proto.PowerPart(nil), p.pmParts...)
}

func nvidiaSMIPaths() []string { return nil }
