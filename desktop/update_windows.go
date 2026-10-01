//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Desktop releases are GitHub releases tagged "desktop-vX.Y.Z" with an installer asset named
// "ApexRMM-Desktop-Setup-X.Y.Z.exe".
const releasesAPI = "https://api.github.com/repos/hardynetworks/apex-rmm/releases?per_page=30"

type release struct {
	Version  string
	PageURL  string
	AssetURL string
	Asset    string
}

func (a *app) updateLoop() {
	time.Sleep(20 * time.Second)
	for {
		a.checkUpdate(false)
		time.Sleep(12 * time.Hour)
	}
}

func (a *app) checkUpdate(interactive bool) {
	if version == "dev" {
		if interactive {
			a.dispatch(func() {
				messageBox(0, "This is a development build; updates are not checked.", appName, mbOK|mbIconInfo)
			})
		}
		return
	}
	r, err := latestRelease()
	if err != nil {
		log.Printf("update check: %v", err)
		if interactive {
			a.dispatch(func() { messageBox(0, "Couldn't check for updates: "+err.Error(), appName, mbOK|mbIconError) })
		}
		return
	}
	if r == nil || !newer(r.Version, version) {
		if interactive {
			a.dispatch(func() { messageBox(0, "You have the latest version ("+version+").", appName, mbOK|mbIconInfo) })
		}
		return
	}
	a.dispatch(func() {
		first := a.update == nil || a.update.Version != r.Version
		a.update = r
		if first || interactive {
			a.notifyURL = "#update"
			a.tray.balloon(appName+" "+r.Version+" is available", "Click here to install it.")
		}
	})
}

func latestRelease() (*release, error) {
	req, _ := http.NewRequest("GET", releasesAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ApexRMM-Desktop/"+version)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	var best *release
	for _, r := range rels {
		if r.Draft || r.Prerelease || !strings.HasPrefix(r.Tag, "desktop-v") {
			continue
		}
		v := strings.TrimPrefix(r.Tag, "desktop-v")
		for _, as := range r.Assets {
			if strings.HasPrefix(as.Name, "ApexRMM-Desktop-Setup-") && strings.HasSuffix(as.Name, ".exe") {
				if best == nil || newer(v, best.Version) {
					best = &release{Version: v, PageURL: r.HTMLURL, AssetURL: as.URL, Asset: as.Name}
				}
			}
		}
	}
	return best, nil
}

func parseVer(v string) [3]int {
	var out [3]int
	v = strings.SplitN(strings.TrimPrefix(v, "v"), "-", 2)[0]
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func newer(a, b string) bool {
	x, y := parseVer(a), parseVer(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// installUpdate downloads the new installer and runs it silently, but only if it carries a valid
// Authenticode signature. Unsigned builds open the release page instead.
func (a *app) installUpdate() {
	r := a.update
	if r == nil {
		return
	}
	go func() {
		path, err := download(r.AssetURL, r.Asset)
		if err != nil {
			log.Printf("update download: %v", err)
			a.dispatch(func() { shellOpen(r.PageURL, "") })
			return
		}
		if err := verifySignature(path); err != nil {
			log.Printf("update %s is not validly signed (%v); opening the release page instead", r.Asset, err)
			_ = os.Remove(path)
			a.dispatch(func() { shellOpen(r.PageURL, "") })
			return
		}
		log.Printf("installing update %s", r.Version)
		a.dispatch(func() {
			shellOpen(path, "/S") // the installer restarts the app when it's done
			a.quit()
		})
	}()
}

func download(url, name string) (string, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download: %s", resp.Status)
	}
	dst := filepath.Join(os.TempDir(), filepath.Base(name))
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return dst, err
}

func verifySignature(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	file := &windows.WinTrustFileInfo{Size: uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})), FilePath: p}
	data := &windows.WinTrustData{
		Size:                            uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:                        windows.WTD_UI_NONE,
		RevocationChecks:                windows.WTD_REVOKE_NONE,
		UnionChoice:                     windows.WTD_CHOICE_FILE,
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(file),
	}
	err = windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	_ = windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	return err
}
