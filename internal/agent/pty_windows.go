//go:build windows

package agent

import (
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type conPTY struct {
	hpc     windows.Handle
	in      *os.File
	out     *os.File
	process windows.Handle
	once    sync.Once
}

func (p *conPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *conPTY) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}
func (p *conPTY) Close() error {
	p.once.Do(func() {
		_ = windows.TerminateProcess(p.process, 0)
		_ = p.in.Close()
		go func() {
			windows.ClosePseudoConsole(p.hpc)
			_ = p.out.Close()
			_ = windows.CloseHandle(p.process)
		}()
	})
	return nil
}

func startPTY(shell string, cols, rows int) (ptyProc, error) {
	var cmdline string
	switch shell {
	case "cmd":
		cmdline = "cmd.exe"
	case "pwsh":
		if _, err := exec.LookPath("pwsh.exe"); err == nil {
			cmdline = "pwsh.exe -NoLogo"
			break
		}
		fallthrough
	default:
		cmdline = "powershell.exe -NoLogo -ExecutionPolicy Bypass"
	}
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, err
	}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inR, outW, 0, &hpc); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(hpc), unsafe.Sizeof(hpc)); err != nil {
		return nil, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES
	var pi windows.ProcessInformation
	cl, _ := windows.UTF16PtrFromString(cmdline)
	dir, _ := windows.UTF16PtrFromString(os.Getenv("SystemDrive") + `\`)
	err = windows.CreateProcess(nil, cl, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, nil, dir, &si.StartupInfo, &pi)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	return &conPTY{hpc: hpc, in: os.NewFile(uintptr(inW), "pty-in"), out: os.NewFile(uintptr(outR), "pty-out"), process: pi.Process}, nil
}
