package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hardynetworks/hardy-rmm/internal/proto"
)

func itoa(i int) string { return strconv.Itoa(i) }

// runCmd runs a command and returns combined stdout (stderr appended on error).
func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %v %s", name, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// limitedBuffer keeps at most max bytes.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
			l.truncated = true
		} else {
			l.buf.Write(p)
		}
	} else {
		l.truncated = true
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := strings.ToValidUTF8(l.buf.String(), "?")
	if l.truncated {
		s += "\n[output truncated]"
	}
	return s
}

func interpreter(shell, file string) (string, []string, error) {
	win := runtime.GOOS == "windows"
	switch shell {
	case "powershell":
		if win {
			return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file}, nil
		}
		return "pwsh", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-File", file}, nil
	case "pwsh":
		return "pwsh", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file}, nil
	case "cmd":
		if !win {
			return "", nil, errors.New("cmd scripts only run on Windows")
		}
		return "cmd.exe", []string{"/d", "/c", file}, nil
	case "bash":
		return "bash", []string{file}, nil
	case "sh":
		return "/bin/sh", []string{file}, nil
	case "zsh":
		return "zsh", []string{file}, nil
	case "python":
		for _, p := range []string{"python3", "python", "py"} {
			if path, err := exec.LookPath(p); err == nil {
				return path, []string{file}, nil
			}
		}
		return "", nil, errors.New("python is not installed")
	}
	return "", nil, fmt.Errorf("unsupported shell %q", shell)
}

var scriptExt = map[string]string{"powershell": ".ps1", "pwsh": ".ps1", "cmd": ".bat", "bash": ".sh", "sh": ".sh", "zsh": ".sh", "python": ".py"}

// runScript executes a script and returns its result.
func runScript(rs proto.RunScript) proto.ScriptResult {
	res := proto.ScriptResult{ResultID: rs.ResultID, Started: time.Now().Unix()}
	finish := func(status string, code int, stdout, stderr string) proto.ScriptResult {
		res.Status, res.ExitCode, res.Stdout, res.Stderr, res.Finished = status, code, stdout, stderr, time.Now().Unix()
		return res
	}
	dir, err := os.MkdirTemp("", "hardy-script-")
	if err != nil {
		return finish("error", -1, "", err.Error())
	}
	defer os.RemoveAll(dir)
	body := rs.Body
	var data []byte
	switch rs.Shell {
	case "powershell", "pwsh":
		data = append([]byte{0xEF, 0xBB, 0xBF}, []byte(body)...) // BOM so PS 5.1 reads UTF-8
	case "cmd":
		data = []byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	default:
		data = []byte(strings.ReplaceAll(body, "\r\n", "\n"))
	}
	file := filepath.Join(dir, "script"+scriptExt[rs.Shell])
	if err := os.WriteFile(file, data, 0o700); err != nil {
		return finish("error", -1, "", err.Error())
	}
	name, args, err := interpreter(rs.Shell, file)
	if err != nil {
		return finish("error", -1, "", err.Error())
	}
	timeout := time.Duration(rs.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, append(args, rs.Args...)...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range rs.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	hideWindow(cmd)
	setProcGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	stdout := &limitedBuffer{max: 1 << 20}
	stderr := &limitedBuffer{max: 512 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return finish("timeout", code, stdout.String(), stderr.String()+fmt.Sprintf("\n[timed out after %s]", timeout))
	case err != nil && code == -1:
		return finish("error", code, stdout.String(), stderr.String()+"\n"+err.Error())
	case code != 0:
		return finish("failed", code, stdout.String(), stderr.String())
	}
	return finish("success", 0, stdout.String(), stderr.String())
}
