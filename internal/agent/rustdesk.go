package agent

import (
	"errors"
	"regexp"
	"runtime"
	"strings"

	"github.com/hardynetworks/apex-rmm/internal/proto"
)

const rustdeskWindows = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$cfg = '__CONFIG__'
$pw = '__PASSWORD__'
$exe = Join-Path $env:ProgramFiles 'RustDesk\rustdesk.exe'
if (-not (Test-Path $exe)) {
  Write-Output 'Installing RustDesk...'
  $rel = Invoke-RestMethod -UseBasicParsing -Uri 'https://api.github.com/repos/rustdesk/rustdesk/releases/latest'
  $asset = $rel.assets | Where-Object { $_.name -match 'x86_64\.exe$' -and $_.name -notmatch 'sciter' } | Select-Object -First 1
  if (-not $asset) { throw 'Could not find a RustDesk Windows installer in the latest release' }
  $tmp = Join-Path $env:TEMP 'rustdesk-setup.exe'
  Invoke-WebRequest -UseBasicParsing -Uri $asset.browser_download_url -OutFile $tmp
  Start-Process -FilePath $tmp -ArgumentList '--silent-install' -Wait
  for ($i = 0; $i -lt 60 -and -not (Test-Path $exe); $i++) { Start-Sleep -Seconds 2 }
  if (-not (Test-Path $exe)) { throw 'RustDesk install did not complete' }
  Start-Sleep -Seconds 15
}
if (-not (Get-Service -Name 'RustDesk' -ErrorAction SilentlyContinue)) {
  Start-Process -FilePath $exe -ArgumentList '--install-service' -Wait
  Start-Sleep -Seconds 15
}
Start-Process -FilePath $exe -ArgumentList "--config $cfg" -Wait
Start-Process -FilePath $exe -ArgumentList "--password $pw" -Wait
Start-Sleep -Seconds 3
$id = (& $exe --get-id | Out-String).Trim()
Write-Output "RUSTDESK_ID=$id"
`

const rustdeskLinux = `set -e
CFG='__CONFIG__'
PW='__PASSWORD__'
if ! command -v rustdesk >/dev/null 2>&1; then
  echo "Installing RustDesk..."
  case "$(uname -m)" in x86_64|amd64) A=x86_64 ;; aarch64|arm64) A=aarch64 ;; *) echo "unsupported arch"; exit 1 ;; esac
  URLS=$(curl -fsSL https://api.github.com/repos/rustdesk/rustdesk/releases/latest | grep -o '"browser_download_url": *"[^"]*"' | cut -d'"' -f4)
  TMP=$(mktemp -d)
  if command -v apt-get >/dev/null 2>&1; then
    U=$(echo "$URLS" | grep -E "rustdesk-[0-9.]+-$A\.deb$" | head -n1)
    curl -fsSL "$U" -o "$TMP/rustdesk.deb"
    DEBIAN_FRONTEND=noninteractive apt-get update -q || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$TMP/rustdesk.deb"
  elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
    U=$(echo "$URLS" | grep -E "\.$A\.rpm$" | head -n1)
    curl -fsSL "$U" -o "$TMP/rustdesk.rpm"
    if command -v dnf >/dev/null 2>&1; then dnf install -y "$TMP/rustdesk.rpm"; else yum install -y "$TMP/rustdesk.rpm"; fi
  elif command -v zypper >/dev/null 2>&1; then
    U=$(echo "$URLS" | grep -E "$A-suse\.rpm$" | head -n1)
    curl -fsSL "$U" -o "$TMP/rustdesk.rpm"
    zypper --non-interactive install --allow-unsigned-rpm "$TMP/rustdesk.rpm"
  else
    echo "No supported package manager (apt/dnf/yum/zypper)"; exit 1
  fi
  rm -rf "$TMP"
fi
systemctl enable --now rustdesk >/dev/null 2>&1 || true
sleep 5
rustdesk --config "$CFG" || true
rustdesk --password "$PW" || true
systemctl restart rustdesk >/dev/null 2>&1 || true
sleep 5
echo "RUSTDESK_ID=$(rustdesk --get-id 2>/dev/null | tail -n1)"
`

const rustdeskMac = `set -e
CFG='__CONFIG__'
PW='__PASSWORD__'
APP=/Applications/RustDesk.app
if [ ! -d "$APP" ]; then
  echo "Installing RustDesk..."
  case "$(uname -m)" in arm64) A=aarch64 ;; *) A=x86_64 ;; esac
  U=$(curl -fsSL https://api.github.com/repos/rustdesk/rustdesk/releases/latest | grep -o '"browser_download_url": *"[^"]*"' | cut -d'"' -f4 | grep -E "rustdesk-[0-9.]+-$A\.dmg$" | head -n1)
  TMP=$(mktemp -d)
  curl -fsSL "$U" -o "$TMP/rustdesk.dmg"
  hdiutil attach -nobrowse -quiet -mountpoint "$TMP/mnt" "$TMP/rustdesk.dmg"
  cp -R "$TMP/mnt/RustDesk.app" /Applications/
  hdiutil detach -quiet "$TMP/mnt" || true
  rm -rf "$TMP"
fi
RD="$APP/Contents/MacOS/RustDesk"
"$RD" --config "$CFG" || true
"$RD" --password "$PW" || true
sleep 2
echo "RUSTDESK_ID=$("$RD" --get-id 2>/dev/null | tail -n1)"
echo "NOTE: on macOS, open RustDesk once on the Mac and grant Screen Recording + Accessibility."
`

var rustdeskIDRe = regexp.MustCompile(`RUSTDESK_ID=\s*(\d{6,})`)
var safeArg = regexp.MustCompile(`^[A-Za-z0-9_=+/\-]*$`)

// provisionRustDesk installs RustDesk (if needed) and points it at the self-hosted server.
func provisionRustDesk(p proto.RustDeskProvision) (proto.RustDeskResult, error) {
	if !safeArg.MatchString(p.ConfigB64) || !safeArg.MatchString(p.Password) {
		return proto.RustDeskResult{}, errors.New("invalid provisioning parameters")
	}
	var shell, script string
	switch runtime.GOOS {
	case "windows":
		shell, script = "powershell", rustdeskWindows
	case "darwin":
		shell, script = "bash", rustdeskMac
	default:
		shell, script = "bash", rustdeskLinux
	}
	script = strings.NewReplacer("__CONFIG__", p.ConfigB64, "__PASSWORD__", p.Password).Replace(script)
	res := runScript(proto.RunScript{Shell: shell, Body: script, Timeout: 540})
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	r := proto.RustDeskResult{Output: out}
	if m := rustdeskIDRe.FindStringSubmatch(out); m != nil {
		r.ID = m[1]
	}
	if res.Status != "success" && r.ID == "" {
		return r, errors.New("RustDesk provisioning failed: " + tail(out, 800))
	}
	return r, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
