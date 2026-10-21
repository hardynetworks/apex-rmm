package vsphere

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/vmware/govmomi/guest"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// GuestAuth is a guest OS account used for VMware Tools guest operations.
// It is never stored: callers keep it in memory only for the duration of a job.
type GuestAuth struct {
	Username string
	Password string
}

// InstallResult is the outcome of the installer run inside a guest.
type InstallResult struct {
	ExitCode int32
	Log      string // tail of the installer's output
}

const (
	windowsLog = `C:\Windows\Temp\apex-deploy.log`
	linuxLog   = `/tmp/apex-deploy.log`
	// guest operations hand the command to a shell/CreateProcess, so the URL is
	// restricted to characters that are inert in both.
	safeURL = `^https?://[A-Za-z0-9._:/\-]+$`
)

var safeURLRe = regexp.MustCompile(safeURL)

// ErrUnsupportedGuest is returned for guests Apex cannot install into this way.
var ErrUnsupportedGuest = errors.New("only Windows and Linux guests are supported")

// guestProc is the part of the guest-operations API used by the installer.
// It exists so the orchestration can be tested without a real VMware Tools.
type guestProc interface {
	Start(ctx context.Context, program, args string) (int64, error)
	// Wait blocks until the process has exited and returns its exit code.
	Wait(ctx context.Context, pid int64) (int32, error)
	ReadFile(ctx context.Context, path string, max int64) (string, error)
	DeleteFile(ctx context.Context, path string) error
}

// WindowsInstallCommand returns the program and arguments that run the Apex
// PowerShell installer (served by the Apex server) and log to windowsLog.
func WindowsInstallCommand(installURL string) (program, args string, err error) {
	if !safeURLRe.MatchString(installURL) {
		return "", "", fmt.Errorf("install URL %q contains unexpected characters", installURL)
	}
	script := `$ErrorActionPreference='Stop';` +
		`[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12;` +
		`$log='` + windowsLog + `';` +
		`try{(Invoke-RestMethod -UseBasicParsing -Uri '` + installURL + `' | Invoke-Expression) *> $log;exit 0}` +
		`catch{$_ | Out-File -Append $log;exit 1}`
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		`-NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ` + base64.StdEncoding.EncodeToString(b), nil
}

// LinuxInstallCommand returns the program and arguments that download and run
// the Apex shell installer. With sudo=true the installer runs through
// "sudo -n" (the account needs passwordless sudo); root needs no sudo.
func LinuxInstallCommand(installURL string, sudo bool) (program, args string, err error) {
	if !safeURLRe.MatchString(installURL) {
		return "", "", fmt.Errorf("install URL %q contains unexpected characters", installURL)
	}
	pre := ""
	if sudo {
		pre = "sudo -n "
	}
	inner := `{ F=/tmp/apex-install.$$.sh; ` +
		`if command -v curl >/dev/null 2>&1; then curl -fsSL ` + installURL + ` -o $F; else wget -qO $F ` + installURL + `; fi && ` +
		pre + `sh $F; rc=$?; rm -f $F; exit $rc; } >` + linuxLog + ` 2>&1`
	return "/bin/sh", "-c '" + inner + "'", nil
}

// InstallAgent runs the Apex installer inside a guest VM through VMware Tools.
// installURL is the one-line installer served by the Apex server
// (e.g. https://rmm.example.com/install/<token>/windows.ps1).
func (c *Conn) InstallAgent(ctx context.Context, vmRef, osFamily string, auth GuestAuth, installURL string, timeout time.Duration) (*InstallResult, error) {
	vm := object.NewVirtualMachine(c.c.Client, types.ManagedObjectReference{Type: "VirtualMachine", Value: vmRef})
	om := guest.NewOperationsManager(c.c.Client, vm.Reference())
	pm, err := om.ProcessManager(ctx)
	if err != nil {
		return nil, guestErr(err)
	}
	fm, err := om.FileManager(ctx)
	if err != nil {
		return nil, guestErr(err)
	}
	g := &govGuest{c: c, pm: pm, fm: fm, auth: &types.NamePasswordAuthentication{Username: auth.Username, Password: auth.Password}}
	return runInstall(ctx, g, osFamily, auth.Username, installURL, timeout)
}

func runInstall(ctx context.Context, g guestProc, osFamily, username, installURL string, timeout time.Duration) (*InstallResult, error) {
	var program, args, logPath string
	var err error
	switch osFamily {
	case "windows":
		program, args, err = WindowsInstallCommand(installURL)
		logPath = windowsLog
	case "linux":
		root := strings.EqualFold(username, "root")
		program, args, err = LinuxInstallCommand(installURL, !root)
		logPath = linuxLog
	default:
		return nil, ErrUnsupportedGuest
	}
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pid, err := g.Start(rctx, program, args)
	if err != nil {
		return nil, err
	}
	exit, werr := g.Wait(rctx, pid)
	res := &InstallResult{ExitCode: exit}
	// Read the log even if waiting failed: it usually says why.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer lcancel()
	if log, err := g.ReadFile(lctx, logPath, 16<<10); err == nil {
		res.Log = tail(strings.TrimSpace(log), 4000)
	}
	_ = g.DeleteFile(lctx, logPath)
	if werr != nil {
		return res, werr
	}
	return res, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---- govmomi-backed guestProc ----

type govGuest struct {
	c    *Conn
	pm   *guest.ProcessManager
	fm   *guest.FileManager
	auth *types.NamePasswordAuthentication
}

func (g *govGuest) Start(ctx context.Context, program, args string) (int64, error) {
	pid, err := g.pm.StartProgram(ctx, g.auth, &types.GuestProgramSpec{ProgramPath: program, Arguments: args})
	return pid, guestErr(err)
}

func (g *govGuest) Wait(ctx context.Context, pid int64) (int32, error) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	missing := 0
	for {
		procs, err := g.pm.ListProcesses(ctx, g.auth, []int64{pid})
		if err != nil {
			return -1, guestErr(err)
		}
		if len(procs) == 0 {
			if missing++; missing >= 5 {
				return -1, errors.New("the installer process disappeared before reporting an exit code")
			}
		} else if procs[0].EndTime != nil {
			return procs[0].ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, errors.New("timed out waiting for the installer to finish")
		case <-t.C:
		}
	}
}

func (g *govGuest) ReadFile(ctx context.Context, path string, max int64) (string, error) {
	info, err := g.fm.InitiateFileTransferFromGuest(ctx, g.auth, path)
	if err != nil {
		return "", err
	}
	u, err := g.fm.TransferURL(ctx, info.Url)
	if err != nil {
		return "", err
	}
	rc, _, err := g.c.c.Client.Download(ctx, u, &soap.DefaultDownload)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max))
	return string(b), err
}

func (g *govGuest) DeleteFile(ctx context.Context, path string) error {
	return g.fm.DeleteFile(ctx, g.auth, path)
}

// guestErr translates guest-operation faults into advice an operator can act on.
func guestErr(err error) error {
	if err == nil {
		return nil
	}
	if soap.IsVimFault(err) {
		switch soap.ToVimFault(err).(type) {
		case *types.InvalidGuestLogin:
			return errors.New("the guest rejected the username/password (on Windows use .\\Administrator for a local account or DOMAIN\\user)")
		case *types.GuestOperationsUnavailable:
			return errors.New("guest operations are unavailable: VMware Tools is not running or is too old")
		case *types.GuestPermissionDenied:
			return errors.New("the guest account is not allowed to run programs (Windows: it must be an administrator)")
		case *types.NoPermission:
			return errors.New("the vCenter account lacks the Guest operations privileges (VirtualMachine.GuestOperations.*)")
		case *types.InvalidPowerState:
			return errors.New("the VM is not powered on")
		}
	}
	return cleanErr(err)
}
