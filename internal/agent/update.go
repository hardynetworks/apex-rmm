package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func fileSHA256(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

// selfUpdate downloads a new agent binary, verifies it and swaps it into place.
func selfUpdate(url, want string) error {
	dst := InstallPath()
	if strings.EqualFold(fileSHA256(dst), want) {
		return fmt.Errorf("already up to date")
	}
	resp, err := httpClient().Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	tmp := dst + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch (got %s)", got)
	}
	if err := checkRuns(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(dst + ".old")
		if err := os.Rename(dst, dst+".old"); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	log.Printf("agent updated; restarting")
	return nil
}

// checkRuns makes sure the downloaded agent can start on this system before it replaces the
// running one, so a build that can't run here (wrong C library, wrong CPU) never takes the agent
// offline. Only on Linux, where builds differ by C library.
func checkRuns(p string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the new agent build can't run on this system (%v: %s); keeping the current one", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// platformArch is the arch name the server knows this build by: the static Linux build
// asks for its own variant ("amd64-static").
func platformArch() string {
	if buildVariant != "" {
		return runtime.GOARCH + "-" + buildVariant
	}
	return runtime.GOARCH
}

// latestBuild asks the server for the current agent build for this platform.
func (a *Agent) latestBuild(ctx context.Context) (url, sha string, err error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+"/api/agent/version?os="+runtime.GOOS+"&arch="+platformArch(), nil)
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var v struct {
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if resp.StatusCode != 200 || v.SHA256 == "" {
		return "", "", fmt.Errorf("no agent build for %s/%s on the server", runtime.GOOS, platformArch())
	}
	return v.URL, v.SHA256, nil
}

// restartSelf exits so the service manager restarts us on the new binary.
func restartSelf() {
	go func() {
		time.Sleep(2 * time.Second)
		os.Exit(1) // non-zero so Windows SCM recovery / systemd / launchd restart us
	}()
}

// updateLoop checks the server for a newer agent build every 6 hours.
func (a *Agent) updateLoop(ctx context.Context) {
	t := time.NewTimer(3 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(6 * time.Hour)
		self, _ := os.Executable()
		if !samePath(self, InstallPath()) {
			continue // not running from the installed location (dev mode)
		}
		url, sha, err := a.latestBuild(ctx)
		if err != nil || strings.EqualFold(sha, fileSHA256(InstallPath())) {
			continue
		}
		if err := selfUpdate(url, sha); err != nil {
			log.Printf("auto-update failed: %v", err)
			continue
		}
		restartSelf()
		return
	}
}
