//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"os/user"
	"syscall"

	"github.com/creack/pty"
)

type unixPTY struct {
	f   *os.File
	cmd *exec.Cmd
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *unixPTY) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}
func (p *unixPTY) Close() error {
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGHUP)
		_ = p.cmd.Process.Kill()
	}
	err := p.f.Close()
	go p.cmd.Wait()
	return err
}

func startPTY(shell string, cols, rows int) (ptyProc, error) {
	candidates := []string{shell, "bash", "zsh", "sh"}
	if shell == "powershell" || shell == "pwsh" {
		candidates = []string{"pwsh", "bash", "sh"}
	}
	var path string
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		path = "/bin/sh"
	}
	args := []string{}
	if path != "/bin/sh" {
		args = append(args, "-l")
	}
	cmd := exec.Command(path, args...)
	home := "/root"
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "HOME="+home, "LANG=en_US.UTF-8")
	f, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}, &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		return nil, err
	}
	return &unixPTY{f: f, cmd: cmd}, nil
}
