//go:build !windows

package desktop

import (
	"context"
	"os/exec"
)

func cmdOutput(ctx context.Context, cmd *exec.Cmd) (string, error) {
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return "", ctx.Err()
	}
}
