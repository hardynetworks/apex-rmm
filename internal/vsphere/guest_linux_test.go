//go:build linux

package vsphere

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the real Linux install command line in a shell, with a stand-in sudo that only
// lets the installer run when the right password arrives on stdin.
func TestLinuxInstallCommandRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the "installer": proves it ran, and that the password file was already gone
		w.Write([]byte("echo installer-ran; ls /tmp/.apex-sudo-test* 2>/dev/null && echo PASSFILE-STILL-THERE; exit 0\n"))
	}))
	defer srv.Close()
	bin := t.TempDir()
	fakeSudo := `#!/bin/sh
[ "$1" = "-S" ] || { echo "sudo: a password is required"; exit 1; }
IFS= read -r pw
[ "$pw" = 'pa ss$word' ] || { echo "sudo: 1 incorrect password attempt"; exit 1; }
shift 3
exec "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(password string) string {
		pf := "/tmp/.apex-sudo-test" + strings.ReplaceAll(t.Name(), "/", "")
		if err := os.WriteFile(pf, []byte(password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(pf)
		prog, args, err := LinuxInstallCommand(srv.URL+"/install/t/linux.sh", true, pf)
		if err != nil {
			t.Fatal(err)
		}
		// VMware Tools runs "<program> <arguments>" through the shell
		cmd := exec.Command("/bin/sh", "-c", prog+" "+args)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		_ = cmd.Run()
		out, _ := os.ReadFile(linuxLog)
		os.Remove(linuxLog)
		if _, err := os.Stat(pf); err == nil {
			t.Fatal("password file left behind")
		}
		return string(out)
	}
	if out := run("pa ss$word"); !strings.Contains(out, "installer-ran") || strings.Contains(out, "PASSFILE-STILL-THERE") {
		t.Fatalf("right password: %q", out)
	}
	if out := run("nope"); !strings.Contains(out, "incorrect password") || strings.Contains(out, "installer-ran") {
		t.Fatalf("wrong password: %q", out)
	}
}
