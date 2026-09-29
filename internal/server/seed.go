package server

import (
	"context"
	"log/slog"
)

type seedScript struct {
	name, desc, cat, shell string
	platforms              []string
	body                   string
}

var defaultScripts = []seedScript{
	{"System info", "OS, uptime and hardware summary", "Diagnostics", "powershell", []string{"windows"},
		`Get-ComputerInfo -Property CsName, OsName, OsVersion, OsBuildNumber, CsManufacturer, CsModel, CsTotalPhysicalMemory, OsLastBootUpTime | Format-List`},
	{"System info", "OS, uptime and hardware summary", "Diagnostics", "bash", []string{"linux", "darwin"},
		`uname -a
echo
uptime
echo
if command -v lsb_release >/dev/null; then lsb_release -a 2>/dev/null; elif [ -f /etc/os-release ]; then cat /etc/os-release; else sw_vers; fi
echo
df -h`},
	{"Pending Windows updates", "Lists updates available from Windows Update (no install)", "Patching", "powershell", []string{"windows"},
		`$s = New-Object -ComObject Microsoft.Update.Session
$r = $s.CreateUpdateSearcher().Search("IsInstalled=0 and IsHidden=0")
if ($r.Updates.Count -eq 0) { "No pending updates." } else { $r.Updates | ForEach-Object { "{0}  [{1}]" -f $_.Title, ($_.KBArticleIDs -join ',') } }`},
	{"Clear temp files", "Removes files older than 7 days from temp folders", "Maintenance", "powershell", []string{"windows"},
		`$paths = @("$env:windir\Temp", "C:\Users\*\AppData\Local\Temp")
$before = (Get-PSDrive C).Free
foreach ($p in $paths) { Get-ChildItem $p -Recurse -Force -ErrorAction SilentlyContinue | Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-7) } | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue }
$freed = ((Get-PSDrive C).Free - $before) / 1MB
"Freed {0:N0} MB" -f $freed`},
	{"Flush DNS cache", "Clears the DNS resolver cache", "Network", "cmd", []string{"windows"}, `ipconfig /flushdns`},
	{"apt upgrade", "Updates packages on Debian/Ubuntu", "Patching", "bash", []string{"linux"},
		`export DEBIAN_FRONTEND=noninteractive
apt-get update -q && apt-get -y -q upgrade
[ -f /var/run/reboot-required ] && echo "REBOOT REQUIRED" || true`},
	{"Top processes", "Top 15 processes by CPU", "Diagnostics", "sh", []string{"linux", "darwin"}, `ps aux | sort -nrk 3,3 | head -n 15`},
	{"macOS software updates", "Lists available macOS updates", "Patching", "zsh", []string{"darwin"}, `softwareupdate -l 2>&1`},
}

func (s *Server) seed(ctx context.Context) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM clients`).Scan(&n); err == nil && n == 0 {
		var id string
		if err := s.db.QueryRow(ctx, `INSERT INTO clients (name) VALUES ($1) RETURNING id`, s.conf().CompanyName).Scan(&id); err == nil {
			_, _ = s.db.Exec(ctx, `INSERT INTO sites (client_id, name) VALUES ($1, 'Main')`, id)
		}
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM alert_policies`).Scan(&n); err == nil && n == 0 {
		_, _ = s.db.Exec(ctx, `INSERT INTO alert_policies (name, metric, operator, threshold, duration_minutes, severity) VALUES
			('High CPU', 'cpu', '>', 90, 10, 'warning'),
			('High memory', 'mem', '>', 92, 10, 'warning'),
			('Disk almost full', 'disk', '>', 90, 5, 'critical'),
			('Device offline', 'offline', '>', 0, 15, 'warning')`)
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM scripts`).Scan(&n); err == nil && n == 0 {
		for _, sc := range defaultScripts {
			if _, err := s.db.Exec(ctx, `INSERT INTO scripts (name, description, category, shell, platforms, body, created_by) VALUES ($1,$2,$3,$4,$5,$6,'system')`,
				sc.name, sc.desc, sc.cat, sc.shell, sc.platforms, sc.body); err != nil {
				slog.Warn("seed script", "err", err)
			}
		}
	}
}
