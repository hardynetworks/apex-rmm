package agent

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// LogPath returns the log file for a given component ("agent", "desktop").
func LogPath(name string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(filepath.Dir(ConfigPath()), name+".log")
	case "darwin":
		return "/Library/Logs/ApexRMM/" + name + ".log"
	default:
		return "/var/log/apex-agent/" + name + ".log"
	}
}

// SetupLogging sends log output to a size-capped file (and stderr).
func SetupLogging(name string) {
	p := LogPath(name)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags)
}
