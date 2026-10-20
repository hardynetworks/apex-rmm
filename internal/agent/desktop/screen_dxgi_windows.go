//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DXGI Desktop Duplication (Windows 8+): the compositor hands us the desktop as a GPU texture,
// which is far faster than GDI BitBlt (a 4K screen in a few milliseconds) and makes 60+ fps
// possible. It needs a display adapter (not available in some VMs or RDP sessions) and has to
// be recreated when the desktop switches (UAC, lock screen), so GDI remains the fallback.
//
// COM is called through vtables; the slot numbers below come from the Windows SDK headers
// (dxgi.h, dxgi1_2.h, d3d11.h).
var (
	dxgiDLL  = windows.NewLazySystemDLL("dxgi.dll")
	d3d11DLL = windows.NewLazySystemDLL("d3d11.dll")

	pCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
	pD3D11CreateDevice  = d3d11DLL.NewProc("D3D11CreateDevice")

	iidDXGIFactory1   = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidDXGIOutput1    = windows.GUID{Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83, Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidD3D11Texture2D = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

const (
	vtQueryInterface = 0
	vtRelease        = 2

	vtFactoryEnumAdapters1 = 12 // IDXGIFactory1
	vtAdapterEnumOutputs   = 7  // IDXGIAdapter
	vtOutputGetDesc        = 7  // IDXGIOutput
	vtOutput1Duplicate     = 22 // IDXGIOutput1::DuplicateOutput
	vtDupAcquireNextFrame  = 8  // IDXGIOutputDuplication
	vtDupReleaseFrame      = 14
	vtDeviceCreateTexture  = 5  // ID3D11Device::CreateTexture2D
	vtContextMap           = 14 // ID3D11DeviceContext
	vtContextUnmap         = 15
	vtContextCopyResource  = 47

	dxgiErrNotFound      = 0x887A0002
	dxgiErrWaitTimeout   = 0x887A0027
	dxgiFormatBGRA       = 87
	d3d11UsageStaging    = 3
	d3d11CPUAccessRead   = 0x20000
	d3d11MapRead         = 1
	d3d11CreateBGRA      = 0x20
	d3d11SDKVersion      = 7
	d3dDriverTypeUnknown = 0
)

func comCall(obj uintptr, slot int, args ...uintptr) uint32 {
	vtbl := *(*uintptr)(ptrFrom(obj))
	fn := *(*uintptr)(ptrFrom(vtbl + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return uint32(r)
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, vtRelease)
	}
}

func failed(hr uint32) bool { return int32(hr) < 0 }

type dxgiOutputDesc struct {
	DeviceName [32]uint16
	Left, Top  int32
	Right      int32
	Bottom     int32
	Attached   int32
	Rotation   uint32
	Monitor    uintptr
}

type d3d11Texture2DDesc struct {
	Width, Height, MipLevels, ArraySize, Format uint32
	SampleCount, SampleQuality                  uint32
	Usage, BindFlags, CPUAccessFlags, Misc      uint32
}

type d3d11Mapped struct {
	Data       uintptr
	RowPitch   uint32
	DepthPitch uint32
}

type dxgiFrameInfo struct {
	LastPresentTime     int64
	LastMouseUpdateTime int64
	AccumulatedFrames   uint32
	RectsCoalesced      int32
	ProtectedMasked     int32
	PointerX, PointerY  int32
	PointerVisible      int32
	MetadataSize        uint32
	PointerShapeSize    uint32
	_                   [16]byte // slack
}

type dxgiDup struct {
	dev, ctx, dup, staging uintptr
	w, h                   int
	have                   bool // dst holds a complete image from DXGI
}

// newDXGIDup duplicates the output whose desktop rectangle is d.
func newDXGIDup(d Display) (*dxgiDup, error) {
	if err := pCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	if err := pD3D11CreateDevice.Find(); err != nil {
		return nil, err
	}
	var factory uintptr
	if r, _, _ := pCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); failed(uint32(r)) {
		return nil, fmt.Errorf("CreateDXGIFactory1: %#x", uint32(r))
	}
	defer comRelease(factory)
	for ai := uint32(0); ; ai++ {
		var adapter uintptr
		if hr := comCall(factory, vtFactoryEnumAdapters1, uintptr(ai), uintptr(unsafe.Pointer(&adapter))); hr == dxgiErrNotFound || failed(hr) {
			break
		}
		x, err := duplicateOnAdapter(adapter, d)
		comRelease(adapter)
		if x != nil || err != nil {
			return x, err
		}
	}
	return nil, errors.New("no DXGI output matches this display")
}

func duplicateOnAdapter(adapter uintptr, d Display) (*dxgiDup, error) {
	for oi := uint32(0); ; oi++ {
		var output uintptr
		if hr := comCall(adapter, vtAdapterEnumOutputs, uintptr(oi), uintptr(unsafe.Pointer(&output))); hr == dxgiErrNotFound || failed(hr) {
			return nil, nil
		}
		var desc dxgiOutputDesc
		hr := comCall(output, vtOutputGetDesc, uintptr(unsafe.Pointer(&desc)))
		match := !failed(hr) && int(desc.Left) == d.X && int(desc.Top) == d.Y &&
			int(desc.Right-desc.Left) == d.W && int(desc.Bottom-desc.Top) == d.H
		if !match {
			comRelease(output)
			continue
		}
		defer comRelease(output)
		if desc.Rotation > 1 { // rotated displays come out sideways: let GDI handle them
			return nil, errors.New("rotated display")
		}
		var output1 uintptr
		if hr := comCall(output, vtQueryInterface, uintptr(unsafe.Pointer(&iidDXGIOutput1)), uintptr(unsafe.Pointer(&output1))); failed(hr) {
			return nil, fmt.Errorf("IDXGIOutput1: %#x", hr)
		}
		defer comRelease(output1)
		x := &dxgiDup{w: d.W, h: d.H}
		var level uint32
		if r, _, _ := pD3D11CreateDevice.Call(adapter, d3dDriverTypeUnknown, 0, d3d11CreateBGRA, 0, 0, d3d11SDKVersion,
			uintptr(unsafe.Pointer(&x.dev)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&x.ctx))); failed(uint32(r)) {
			return nil, fmt.Errorf("D3D11CreateDevice: %#x", uint32(r))
		}
		if hr := comCall(output1, vtOutput1Duplicate, x.dev, uintptr(unsafe.Pointer(&x.dup))); failed(hr) {
			x.Close()
			return nil, fmt.Errorf("DuplicateOutput: %#x", hr)
		}
		desc2 := d3d11Texture2DDesc{Width: uint32(d.W), Height: uint32(d.H), MipLevels: 1, ArraySize: 1, Format: dxgiFormatBGRA,
			SampleCount: 1, Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessRead}
		if hr := comCall(x.dev, vtDeviceCreateTexture, uintptr(unsafe.Pointer(&desc2)), 0, uintptr(unsafe.Pointer(&x.staging))); failed(hr) {
			x.Close()
			return nil, fmt.Errorf("CreateTexture2D: %#x", hr)
		}
		return x, nil
	}
}

// capture copies the latest desktop image into dst (w x h, stride w*4). It reports false when
// DXGI has no complete image yet (the caller uses GDI for this frame), and an error when the
// duplication is lost (desktop switch, mode change) and must be recreated.
func (x *dxgiDup) capture(dst *Frame) (bool, error) {
	var info dxgiFrameInfo
	var res uintptr
	hr := comCall(x.dup, vtDupAcquireNextFrame, 0, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&res)))
	if hr == dxgiErrWaitTimeout {
		return x.have, nil // nothing changed since the last frame
	}
	if failed(hr) {
		return false, fmt.Errorf("AcquireNextFrame: %#x", hr)
	}
	defer comCall(x.dup, vtDupReleaseFrame)
	defer comRelease(res)
	if info.AccumulatedFrames == 0 && x.have { // only the pointer moved
		return true, nil
	}
	var tex uintptr
	if hr := comCall(res, vtQueryInterface, uintptr(unsafe.Pointer(&iidD3D11Texture2D)), uintptr(unsafe.Pointer(&tex))); failed(hr) {
		return false, fmt.Errorf("ID3D11Texture2D: %#x", hr)
	}
	comCall(x.ctx, vtContextCopyResource, x.staging, tex)
	comRelease(tex)
	var m d3d11Mapped
	if hr := comCall(x.ctx, vtContextMap, x.staging, 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(&m))); failed(hr) {
		return false, fmt.Errorf("Map: %#x", hr)
	}
	if len(dst.Pix) != x.w*x.h*4 {
		*dst = Frame{W: x.w, H: x.h, Stride: x.w * 4, Pix: make([]byte, x.w*x.h*4)}
	}
	src := unsafe.Slice((*byte)(ptrFrom(m.Data)), int(m.RowPitch)*x.h)
	row := x.w * 4
	for y := 0; y < x.h; y++ {
		copy(dst.Pix[y*row:(y+1)*row], src[y*int(m.RowPitch):y*int(m.RowPitch)+row])
	}
	comCall(x.ctx, vtContextUnmap, x.staging, 0)
	x.have = true
	return true, nil
}

func (x *dxgiDup) Close() {
	comRelease(x.staging)
	comRelease(x.dup)
	comRelease(x.ctx)
	comRelease(x.dev)
	x.staging, x.dup, x.ctx, x.dev = 0, 0, 0, 0
}

// A crash inside a graphics driver takes the helper down with it, so a marker file is kept
// while DXGI is being brought up; if a helper finds one left behind, it sticks to GDI for a day.
func dxgiMarker() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "ApexRMM", "dxgi-starting")
}

func dxgiAllowed() bool {
	if os.Getenv("APEX_NO_DXGI") != "" {
		return false
	}
	if st, err := os.Stat(dxgiMarker()); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return false
	}
	return true
}
