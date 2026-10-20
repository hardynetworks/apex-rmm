package desktop

import (
	"compress/bzip2"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// OpenH264 comes from Cisco: its license covers the H.264 patents only for binaries downloaded
// from Cisco, so the agent fetches the library on first use instead of bundling it. Downloads
// are plain HTTP (that's all ciscobinary.openh264.org offers), so each file is pinned to the
// SHA-256 below, checked again every time before it is loaded. The pins are produced by the
// "OpenH264 pins" workflow, which also checks Cisco's signatures.
const openH264Version = "2.6.0"

type ohPin struct {
	file   string // name on ciscobinary.openh264.org, without ".bz2"
	sha256 string
	size   int64
}

var openH264Pins = map[string]ohPin{
	"linux/amd64":   {"libopenh264-2.6.0-linux64.8.so", "2f0cde7c6a6abcf5cae76942894ea42897fa677bce4ed6c91a24dd1b041d5f04", 1731128},
	"linux/arm64":   {"libopenh264-2.6.0-linux-arm64.8.so", "12e7b33623667cdab0e575170c147b1b36eadb77d0d2aa7ceb5afd3e58902140", 1492120},
	"darwin/arm64":  {"libopenh264-2.6.0-mac-arm64.dylib", "052e98bfcf7a9167d22f3bbb3f5988ef79065591f36af8b52924b22b13624551", 1207136},
	"darwin/amd64":  {"libopenh264-2.6.0-mac-x64.dylib", "e3dc8bc01fe69363f61fd3c02fd27798537a585eadd38cd808f303d1ee505a19", 1381104},
	"windows/amd64": {"openh264-2.6.0-win64.dll", "2076cb5675ec6c1a4c70e7a2a322552f547b6eeed649d6dfcd9e02a543b24691", 978520},
	"windows/arm64": {"openh264-2.6.0-win-arm64.dll", "fb75103938f4f47d119b983e06334df41a803bc72fb5c46e3623f6fea5782732", 810584},
}

var ciscoBase = "http://ciscobinary.openh264.org/"

// codecDir is only writable by root/SYSTEM (the library is loaded by a privileged process).
func codecDir() string {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return filepath.Join(pf, "ApexRMM", "codecs")
	case "darwin":
		return "/usr/local/apex-agent/codecs"
	default:
		return "/var/lib/apex-agent/codecs"
	}
}

var (
	codecMu    sync.Mutex
	codecState string // "", "downloading", "ready", "failed: …"
	codecLib   *ohLib
)

// VideoCodec returns the loaded H.264 encoder library, or nil and a status explaining why it
// isn't available yet ("downloading", an error, …). It never blocks on the network: a missing
// library is fetched in the background.
func VideoCodec() (*ohLib, string) {
	codecMu.Lock()
	defer codecMu.Unlock()
	if codecLib != nil {
		return codecLib, "ready"
	}
	if codecState == "downloading" {
		return nil, codecState
	}
	if p := os.Getenv("APEX_OPENH264"); p != "" { // development / tests
		lib, err := loadOpenH264(p)
		if err != nil {
			codecState = "failed: " + err.Error()
			return nil, codecState
		}
		codecLib = lib
		return lib, "ready"
	}
	pin, ok := openH264Pins[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok || !dlSupported {
		codecState = "unsupported platform"
		return nil, codecState
	}
	if pin.sha256 == "" {
		codecState = "no pinned OpenH264 build for this platform"
		return nil, codecState
	}
	path := filepath.Join(codecDir(), pin.file)
	if err := verifyFile(path, pin); err == nil {
		lib, err := loadOpenH264(path)
		if err != nil {
			codecState = "failed: " + err.Error()
			return nil, codecState
		}
		codecLib = lib
		return lib, "ready"
	}
	if time.Since(lastCodecTry) < time.Minute {
		return nil, codecState
	}
	lastCodecTry = time.Now()
	codecState = "downloading"
	go func() {
		err := fetchOpenH264(path, pin)
		codecMu.Lock()
		defer codecMu.Unlock()
		if err != nil {
			log.Printf("OpenH264 download: %v", err)
			codecState = "failed: " + err.Error()
			return
		}
		codecState = "" // loaded on the next call
	}()
	return nil, codecState
}

var lastCodecTry time.Time

// PrefetchCodec downloads the encoder in the background (called when the agent starts, so the
// first remote session can use video right away).
func PrefetchCodec() {
	go func() {
		pin, ok := openH264Pins[runtime.GOOS+"/"+runtime.GOARCH]
		if !ok || pin.sha256 == "" || !dlSupported {
			return
		}
		path := filepath.Join(codecDir(), pin.file)
		if verifyFile(path, pin) == nil {
			return
		}
		for attempt := 0; attempt < 5; attempt++ {
			if err := fetchOpenH264(path, pin); err == nil {
				log.Printf("OpenH264 %s downloaded from Cisco", openH264Version)
				return
			} else {
				log.Printf("OpenH264 download: %v", err)
			}
			time.Sleep(time.Duration(attempt+1) * time.Minute)
		}
	}()
}

func verifyFile(path string, pin ohPin) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != pin.size || hex.EncodeToString(h.Sum(nil)) != pin.sha256 {
		return errors.New("checksum mismatch")
	}
	return nil
}

var fetchMu sync.Mutex

func fetchOpenH264(path string, pin ohPin) error {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	if verifyFile(path, pin) == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	req, _ := http.NewRequest("GET", ciscoBase+pin.file+".bz2", nil)
	req.Header.Set("User-Agent", "ApexRMM-Agent")
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(bzip2.NewReader(io.LimitReader(resp.Body, 16<<20)), pin.size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != pin.size || hex.EncodeToString(h.Sum(nil)) != pin.sha256 {
		return fmt.Errorf("%s doesn't match its pinned checksum", pin.file)
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	_ = os.Remove(path)
	return os.Rename(tmp.Name(), path)
}
