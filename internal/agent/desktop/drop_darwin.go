//go:build darwin

package desktop

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// macOS: a drop on a Finder window goes into the folder it shows (asked from Finder with
// AppleScript, which needs the one-time "control Finder" approval on the Mac); anywhere else
// goes to the Desktop (or Downloads when it lands on another program's window).
func resolveDrop(gx, gy float64, foreground bool) (dropTarget, error) {
	home := consoleUserHome()
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	script := `tell application "Finder"
	set out to ""
	repeat with w in (every Finder window)
		set b to bounds of w
		if (` + fmt.Sprintf("%d", int(gx)) + ` ≥ item 1 of b) and (` + fmt.Sprintf("%d", int(gx)) + ` ≤ item 3 of b) and (` + fmt.Sprintf("%d", int(gy)) + ` ≥ item 2 of b) and (` + fmt.Sprintf("%d", int(gy)) + ` ≤ item 4 of b) then
			set out to POSIX path of (target of w as alias)
			exit repeat
		end if
	end repeat
	return out
end tell`
	if foreground {
		script = `tell application "System Events" to set app_ to name of first application process whose frontmost is true
if app_ is "Finder" then
	tell application "Finder"
		if (count of Finder windows) > 0 then return POSIX path of (target of front Finder window as alias)
	end tell
	return "desktop"
end if
return ""`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := asConsoleUser("/usr/bin/osascript", "-e", script)
	cmd.Env = append(cmd.Env, "HOME="+home)
	out, err := cmdOutput(ctx, cmd)
	p := strings.TrimSpace(out)
	if err == nil && p != "" && p != "desktop" {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return dropTarget{Path: filepath.Clean(p), Label: filepath.Base(p), Kind: "folder"}, nil
		}
	}
	if foreground && p != "desktop" {
		return dropTarget{Path: filepath.Join(home, "Downloads"), Label: "Downloads", Kind: "app"}, nil
	}
	return dropTarget{Path: filepath.Join(home, "Desktop"), Label: "Desktop", Kind: "desktop"}, nil
}
