package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFetchOpenH264(t *testing.T) {
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 not installed")
	}
	payload := bytes.Repeat([]byte("not really a library "), 5000)
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(payload)
	bz, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lib.so.bz2" {
			w.Write(bz)
			return
		}
		if r.URL.Path == "/evil.so.bz2" {
			c := exec.Command("bzip2", "-c")
			c.Stdin = bytes.NewReader(append(payload[:len(payload)-1], 'X'))
			b, _ := c.Output()
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	old := ciscoBase
	ciscoBase = srv.URL + "/"
	defer func() { ciscoBase = old }()

	sum := sha256.Sum256(payload)
	dir := t.TempDir()
	good := ohPin{file: "lib.so", sha256: hex.EncodeToString(sum[:]), size: int64(len(payload))}
	path := filepath.Join(dir, "codecs", "lib.so")
	if err := fetchOpenH264(path, good); err != nil {
		t.Fatal(err)
	}
	if verifyFile(path, good) != nil {
		t.Fatal("downloaded file doesn't verify")
	}
	// A tampered download is rejected and nothing is left behind.
	bad := ohPin{file: "evil.so", sha256: good.sha256, size: good.size}
	evilPath := filepath.Join(dir, "codecs", "evil.so")
	if err := fetchOpenH264(evilPath, bad); err == nil {
		t.Fatal("tampered download accepted")
	}
	if _, err := os.Stat(evilPath); err == nil {
		t.Fatal("tampered file was kept")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "codecs"))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
