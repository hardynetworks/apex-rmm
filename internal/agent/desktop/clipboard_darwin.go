//go:build darwin

package desktop

import (
	"bytes"
	"crypto/sha256"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// macOS: pbcopy/pbpaste, run as the console user (the helper is root in the user's GUI session).
type macClipboard struct {
	mu      sync.Mutex
	last    [32]byte
	seq     uint64
	checked time.Time
}

func newClipboard() (clipboard, error) { return &macClipboard{}, nil }

func asConsoleUser(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = []string{"LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if uid, gid, ok := consoleOwner(); ok {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
	return cmd
}

func (c *macClipboard) Text() (string, error) {
	out, err := asConsoleUser("/usr/bin/pbpaste").Output()
	if err != nil {
		return "", err
	}
	if len(out) > maxClipBytes {
		out = out[:maxClipBytes]
	}
	return string(out), nil
}

func (c *macClipboard) SetText(s string) error {
	cmd := asConsoleUser("/usr/bin/pbcopy")
	cmd.Stdin = strings.NewReader(s)
	if err := cmd.Run(); err != nil {
		return err
	}
	c.mu.Lock()
	c.last = sha256.Sum256([]byte(s))
	c.mu.Unlock()
	return nil
}

func (c *macClipboard) SetFiles([]string) error { return errUnsupported }

// Seq polls pbpaste (at most every 700 ms) and counts changes.
func (c *macClipboard) Seq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) < 700*time.Millisecond {
		return c.seq
	}
	c.checked = time.Now()
	out, err := asConsoleUser("/usr/bin/pbpaste").Output()
	if err != nil {
		return c.seq
	}
	if h := sha256.Sum256(bytes.TrimRight(out, "\x00")); h != c.last {
		c.last = h
		c.seq++
	}
	return c.seq
}

func (c *macClipboard) Close() {}
