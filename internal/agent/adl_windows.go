//go:build windows

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AMD GPU sensors on Windows through ADL (atiadlxx.dll, installed with the Radeon driver).
// The service tries ADL in its own process first; if the driver gives a session-0 service no
// sensor data, the agent runs itself ("apex-agent adl") in the active user session to read them.

func adlDLLName() string {
	if runtime.GOARCH == "386" {
		return "atiadlxy.dll"
	}
	return "atiadlxx.dll"
}

var (
	adlDLL           = windows.NewLazySystemDLL(adlDLLName())
	adlCreate        = adlDLL.NewProc("ADL2_Main_Control_Create")
	adlDestroy       = adlDLL.NewProc("ADL2_Main_Control_Destroy")
	adlNumAdapters   = adlDLL.NewProc("ADL2_Adapter_NumberOfAdapters_Get")
	adlAdapterInfo   = adlDLL.NewProc("ADL2_Adapter_AdapterInfo_Get")
	adlODCaps        = adlDLL.NewProc("ADL2_Overdrive_Caps")
	adlPMLog         = adlDLL.NewProc("ADL2_New_QueryPMLogData_Get")
	adlOD6Power      = adlDLL.NewProc("ADL2_Overdrive6_CurrentPower_Get")
	adlGcnInfo       = adlDLL.NewProc("ADL2_GcnAsicInfo_Get")
	adlMallocCB      uintptr
	adlMallocCBOnce  sync.Once
	errADLNotPresent = errors.New("no AMD driver (atiadlxx.dll)")
)

// ADL allocates some results with the caller's allocator; it frees nothing we hand it, and the
// calls used here only allocate inside ADL_Main_Control_Create.
func adlMalloc(size int32) uintptr {
	if size <= 0 {
		return 0
	}
	p, _ := windows.LocalAlloc(windows.LPTR, uint32(size))
	return p
}

func adlCall(p *windows.LazyProc, args ...uintptr) int32 {
	if p.Find() != nil {
		return -8 // ADL_ERR_NOT_SUPPORTED
	}
	r, _, _ := p.Call(args...)
	return int32(r)
}

type adlGPU struct {
	adapter adlAdapter
	family  int
	od      int // Overdrive version (5…8), 0 if unknown
}

type adlSession struct {
	ctx  uintptr
	gpus []adlGPU
}

func openADL(connectedOnly bool) (*adlSession, error) {
	if adlDLL.Load() != nil {
		return nil, errADLNotPresent
	}
	if adlCreate.Find() != nil || adlAdapterInfo.Find() != nil {
		return nil, errors.New("ADL is too old on this driver")
	}
	adlMallocCBOnce.Do(func() { adlMallocCB = syscall.NewCallbackCDecl(adlMalloc) })
	conn := uintptr(0)
	if connectedOnly {
		conn = 1
	}
	s := &adlSession{}
	if r := adlCall(adlCreate, adlMallocCB, conn, uintptr(unsafe.Pointer(&s.ctx))); r < 0 || s.ctx == 0 {
		return nil, fmt.Errorf("ADL2_Main_Control_Create failed (%d)", r)
	}
	var n int32
	if r := adlCall(adlNumAdapters, s.ctx, uintptr(unsafe.Pointer(&n))); r < 0 || n <= 0 || n > 64 {
		s.close()
		return nil, fmt.Errorf("no AMD adapters (%d, %d)", r, n)
	}
	buf := make([]byte, int(n)*adlAdapterInfoSize)
	if r := adlCall(adlAdapterInfo, s.ctx, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r < 0 {
		s.close()
		return nil, fmt.Errorf("ADL2_Adapter_AdapterInfo_Get failed (%d)", r)
	}
	for _, a := range parseADLAdapters(buf) {
		g := adlGPU{adapter: a}
		var gcn [5]int32 // CuCount, TexCount, RopCount, ASICFamilyId, ASICRevisionId
		if adlCall(adlGcnInfo, s.ctx, uintptr(a.index), uintptr(unsafe.Pointer(&gcn[0]))) >= 0 {
			g.family = int(gcn[3])
		}
		var sup, en, ver int32
		if adlCall(adlODCaps, s.ctx, uintptr(a.index), uintptr(unsafe.Pointer(&sup)), uintptr(unsafe.Pointer(&en)), uintptr(unsafe.Pointer(&ver))) >= 0 {
			g.od = int(ver)
		}
		s.gpus = append(s.gpus, g)
	}
	if len(s.gpus) == 0 {
		s.close()
		return nil, errors.New("no AMD GPUs")
	}
	return s, nil
}

func (s *adlSession) close() {
	if s.ctx != 0 {
		adlCall(adlDestroy, s.ctx)
		s.ctx = 0
	}
}

func (s *adlSession) read() []adlReading {
	var out []adlReading
	for _, g := range s.gpus {
		r := newADLReading(g.adapter)
		buf := make([]byte, adlPMLogOutputSize)
		got := false
		if adlCall(adlPMLog, s.ctx, uintptr(g.adapter.index), uintptr(unsafe.Pointer(&buf[0]))) >= 0 {
			got = applyPMLog(&r, buf, g.family)
		}
		if r.Watts < 0 && g.od >= 6 && g.od < 8 { // Polaris / Vega with OverdriveN
			var v int32
			if adlCall(adlOD6Power, s.ctx, uintptr(g.adapter.index), adlOD6TotalPower, uintptr(unsafe.Pointer(&v))) >= 0 && v > 0 {
				r.Watts = float64(v>>8) + float64(v&0xff)/256
				if !got {
					r.Source = "od6"
				}
			}
		}
		out = append(out, r)
	}
	return out
}

func hasADLData(rs []adlReading) bool {
	for _, r := range rs {
		if r.Watts >= 0 || r.TempC >= 0 {
			return true
		}
	}
	return false
}

// readADLInProcess opens ADL, reads every AMD GPU once and closes it.
func readADLInProcess() ([]adlReading, error) {
	s, err := openADL(false)
	if err != nil {
		if errors.Is(err, errADLNotPresent) {
			return nil, err
		}
		// some drivers only enumerate adapters driving a display
		if s2, err2 := openADL(true); err2 == nil {
			s, err = s2, nil
		} else {
			return nil, err
		}
	}
	defer s.close()
	return s.read(), nil
}

// PrintADL is "apex-agent adl": the readings as JSON, for the service to collect from the
// user's session.
func PrintADL() int {
	rs, err := readADLInProcess()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(rs)
	return 0
}

// amdSensors keeps ADL readings for a few seconds and remembers how to get them.
type amdSensors struct {
	mu       sync.Mutex
	at       time.Time
	last     []adlReading
	mode     string // "", "inproc", "session", "none"
	retry    time.Time
	sess     *adlSession
	failures int
}

var amdADL = &amdSensors{}

// readings returns the AMD GPUs' sensors (nil when there are none or ADL isn't available).
func (a *amdSensors) readings() []adlReading {
	a.mu.Lock()
	defer a.mu.Unlock()
	ttl := 2 * time.Second
	if a.mode == "session" {
		ttl = 15 * time.Second // a process start per read: not more often than this
	}
	if time.Since(a.at) < ttl {
		return a.last
	}
	a.at = time.Now()
	if a.mode == "none" && time.Now().Before(a.retry) {
		return nil
	}
	if a.mode == "" || a.mode == "none" || a.mode == "inproc" {
		if a.sess == nil {
			s, err := openADL(false)
			if err != nil && !errors.Is(err, errADLNotPresent) {
				s, err = openADL(true)
			}
			if err == nil {
				a.sess = s
			} else if errors.Is(err, errADLNotPresent) {
				a.mode, a.retry, a.last = "none", time.Now().Add(time.Hour), nil
				return nil
			}
		}
		if a.sess != nil {
			rs := a.sess.read()
			if hasADLData(rs) {
				a.mode, a.last, a.failures = "inproc", rs, 0
				return rs
			}
			if a.mode == "inproc" && a.failures < 3 { // a hiccup (driver reset): reopen next time
				a.failures++
				a.sess.close()
				a.sess = nil
				return a.last
			}
			a.sess.close()
			a.sess = nil
		}
	}
	// In-process ADL gave nothing: as a service in session 0, read it from the user's session.
	if rs, err := readADLInSession(); err == nil && hasADLData(rs) {
		a.mode, a.last = "session", rs
		return rs
	}
	a.mode, a.retry, a.last = "none", time.Now().Add(10*time.Minute), nil
	return nil
}

// readADLInSession runs "apex-agent adl" as SYSTEM inside the active console session.
func readADLInSession() ([]adlReading, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &self); err != nil {
		return nil, err
	}
	defer self.Close()
	if !self.IsElevated() {
		return nil, errors.New("not a service")
	}
	var dup windows.Token
	if err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil, windows.SecurityIdentification, windows.TokenPrimary, &dup); err != nil {
		return nil, err
	}
	defer dup.Close()
	session := activeSession()
	if session == 0 {
		return nil, errors.New("no user session")
	}
	if err := windows.SetTokenInformation(dup, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4); err != nil {
		return nil, err
	}
	var r, w windows.Handle
	sa := windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreatePipe(&r, &w, &sa, 0); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(r)
	_ = windows.SetHandleInformation(r, windows.HANDLE_FLAG_INHERIT, 0)
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := &windows.StartupInfo{Desktop: desktop, Flags: windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE, StdOutput: w, StdErr: w}
	si.Cb = uint32(unsafe.Sizeof(*si))
	var pi windows.ProcessInformation
	cl, _ := windows.UTF16PtrFromString(fmt.Sprintf(`"%s" adl`, exe))
	err = windows.CreateProcessAsUser(dup, nil, cl, nil, nil, true, windows.CREATE_NO_WINDOW, nil, nil, si, &pi)
	windows.CloseHandle(w)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	done := make(chan []byte, 1)
	go func() {
		var out []byte
		buf := make([]byte, 4096)
		for {
			var n uint32
			if err := windows.ReadFile(r, buf, &n, nil); err != nil || n == 0 {
				break
			}
			out = append(out, buf[:n]...)
			if len(out) > 64<<10 {
				break
			}
		}
		done <- out
	}()
	select {
	case out := <-done:
		var rs []adlReading
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &rs); err != nil {
			return nil, fmt.Errorf("adl helper: %s", strings.TrimSpace(string(out)))
		}
		return rs, nil
	case <-time.After(10 * time.Second):
		_ = windows.TerminateProcess(pi.Process, 1)
		return nil, errors.New("adl helper timed out")
	}
}
