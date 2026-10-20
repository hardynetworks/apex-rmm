package desktop

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"unsafe"
)

// H.264 encoding with Cisco's OpenH264 (version 2.6.0, loaded at run time; see openh264.go for how
// the library is obtained). Struct offsets below are for OpenH264 2.6.0 on 64-bit targets (same
// layout on Windows, Linux and macOS) and were taken from its headers with offsetof().
const (
	ohVersionMajor, ohVersionMinor = 2, 6

	// SEncParamExt (924 bytes)
	pSize               = 924
	pUsageType          = 0
	pPicWidth           = 4
	pPicHeight          = 8
	pTargetBitrate      = 12
	pRCMode             = 16
	pMaxFrameRate       = 20 // float
	pTemporalLayerNum   = 24
	pSpatialLayerNum    = 28
	pSpatialLayers      = 32 // SSpatialLayerConfig[4], 200 bytes each
	pComplexityMode     = 832
	pIntraPeriod        = 836
	pNumRefFrame        = 840
	pSpsPpsIDStrategy   = 844
	pEntropyCoding      = 856
	pEnableFrameSkip    = 860 // bool
	pMaxBitrate         = 864
	pMaxQp              = 868
	pMultipleThreadIdc  = 892 // unsigned short
	pEnableDenoise      = 908 // bool
	pBackgroundDetect   = 909
	pAdaptiveQuant      = 910
	pSceneChangeDetect  = 912
	pLongTermReference  = 880
	pIdrBitrateRatio    = 916
	slVideoWidth        = 0 // SSpatialLayerConfig
	slVideoHeight       = 4
	slFrameRate         = 8 // float
	slSpatialBitrate    = 12
	slMaxSpatialBitrate = 16
	slProfileIdc        = 20
	slLevelIdc          = 24
	slSliceMode         = 32
	slSliceNum          = 36
	slSignalType        = 184 // bool bVideoSignalTypePresent
	slVideoFormat       = 185
	slFullRange         = 186
	slColorDescription  = 187
	slColorPrimaries    = 188
	slTransfer          = 189
	slColorMatrix       = 190

	// SSourcePicture (80 bytes)
	spSize        = 80
	spColorFormat = 0
	spStride      = 4  // int[4]
	spData        = 24 // uchar*[4]
	spWidth       = 56
	spHeight      = 60
	spTimestamp   = 64

	// SFrameBSInfo (7192 bytes), SLayerBSInfo (56 bytes)
	fbSize       = 7192
	fbLayerNum   = 0
	fbLayers     = 8
	fbFrameType  = 7176
	lbSize       = 56
	lbNalCount   = 16
	lbNalLengths = 24 // int*
	lbBsBuf      = 32 // uchar*

	optFrameRate  = 4
	optBitrate    = 5
	optMaxBitrate = 6
	optTraceLevel = 25

	usageScreen      = 1
	rcBitrateMode    = 1
	videoFormatI420  = 23
	spatialLayerAll  = 4
	frameTypeIDR     = 1
	frameTypeSkip    = 4
	profileBaseline  = 66
	sliceFixedNum    = 1
	vfUndef          = 5
	bt709            = 1
	vtInitializeExt  = 1
	vtGetDefault     = 2
	vtUninitialize   = 3
	vtEncodeFrame    = 4
	vtForceIntra     = 6
	vtSetOption      = 7
	encoderVtblSlots = 9
)

type ohLib struct {
	create, destroy uintptr
	version         string
}

var (
	ohOnce   sync.Once
	ohLoaded *ohLib
	ohErr    error
)

// loadOpenH264 opens the library at path once per process.
func loadOpenH264(path string) (*ohLib, error) {
	ohOnce.Do(func() {
		if !dlSupported || unsafe.Sizeof(uintptr(0)) != 8 {
			ohErr = errors.New("video encoding isn't supported on this platform")
			return
		}
		h, err := dlOpen(path)
		if err != nil {
			ohErr = fmt.Errorf("load OpenH264: %w", err)
			return
		}
		l := &ohLib{}
		if l.create, err = dlSym(h, "WelsCreateSVCEncoder"); err != nil {
			ohErr = err
			return
		}
		if l.destroy, err = dlSym(h, "WelsDestroySVCEncoder"); err != nil {
			ohErr = err
			return
		}
		ver, err := dlSym(h, "WelsGetCodecVersionEx")
		if err != nil {
			ohErr = err
			return
		}
		var v [4]uint32
		dlCall(ver, uintptr(unsafe.Pointer(&v)))
		if v[0] != ohVersionMajor || v[1] != ohVersionMinor {
			ohErr = fmt.Errorf("OpenH264 %d.%d.%d found, %d.%d needed", v[0], v[1], v[2], ohVersionMajor, ohVersionMinor)
			return
		}
		l.version = fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
		ohLoaded = l
	})
	return ohLoaded, ohErr
}

// H264Encoder encodes BGRA frames to H.264 (Annex B, constrained baseline).
type H264Encoder struct {
	lib     *ohLib
	enc     uintptr // ISVCEncoder* (pointer to vtable pointer)
	vt      [encoderVtblSlots]uintptr
	W, H    int // encoded size (even)
	params  []byte
	pic     []byte
	info    []byte
	yuv     []byte
	ts      int64
	bitrate int
	fps     float64
}

// NewH264Encoder creates an encoder for w x h (rounded down to even) at the given bitrate (bit/s)
// and maximum frame rate.
func NewH264Encoder(lib *ohLib, w, h, bitrate int, fps float64) (*H264Encoder, error) {
	w, h = w&^1, h&^1
	if w < 16 || h < 16 {
		return nil, errors.New("screen too small for video")
	}
	e := &H264Encoder{lib: lib, W: w, H: h, bitrate: bitrate, fps: fps}
	var ptr uintptr
	if r := dlCall(lib.create, uintptr(unsafe.Pointer(&ptr))); int32(r) != 0 || ptr == 0 {
		return nil, fmt.Errorf("WelsCreateSVCEncoder failed (%d)", int32(r))
	}
	e.enc = ptr
	vtbl := *(*uintptr)(ptrFrom(ptr))
	for i := range e.vt {
		e.vt[i] = *(*uintptr)(ptrFrom(vtbl + uintptr(i)*8))
	}
	quiet := int32(0)
	e.call(vtSetOption, optTraceLevel, uintptr(unsafe.Pointer(&quiet)))

	e.params = make([]byte, pSize)
	p := e.params
	if r := e.call(vtGetDefault, uintptr(unsafe.Pointer(&p[0]))); r != 0 {
		e.Close()
		return nil, fmt.Errorf("GetDefaultParams failed (%d)", r)
	}
	threads := min(4, max(1, runtime.NumCPU()-1))
	if h < 720 {
		threads = min(threads, 2)
	}
	le := binary.LittleEndian
	le.PutUint32(p[pUsageType:], usageScreen)
	le.PutUint32(p[pPicWidth:], uint32(w))
	le.PutUint32(p[pPicHeight:], uint32(h))
	le.PutUint32(p[pTargetBitrate:], uint32(bitrate))
	le.PutUint32(p[pRCMode:], rcBitrateMode)
	le.PutUint32(p[pMaxFrameRate:], math.Float32bits(float32(fps)))
	le.PutUint32(p[pTemporalLayerNum:], 1)
	le.PutUint32(p[pSpatialLayerNum:], 1)
	le.PutUint32(p[pIntraPeriod:], 0) // keyframes only when asked for
	le.PutUint32(p[pNumRefFrame:], 1)
	le.PutUint32(p[pSpsPpsIDStrategy:], 0) // constant IDs
	le.PutUint32(p[pEntropyCoding:], 0)    // CAVLC (baseline)
	p[pEnableFrameSkip] = 1
	le.PutUint32(p[pMaxBitrate:], uint32(bitrate*2))
	le.PutUint32(p[pMaxQp:], 48) // past this, drop frames rather than turning text into mush
	le.PutUint16(p[pMultipleThreadIdc:], uint16(threads))
	p[pEnableDenoise] = 0
	p[pBackgroundDetect] = 1
	p[pAdaptiveQuant] = 1
	p[pSceneChangeDetect] = 1
	p[pLongTermReference] = 0
	le.PutUint32(p[pIdrBitrateRatio:], 400) // a sharp first frame after a keyframe request
	sl := p[pSpatialLayers:]
	le.PutUint32(sl[slVideoWidth:], uint32(w))
	le.PutUint32(sl[slVideoHeight:], uint32(h))
	le.PutUint32(sl[slFrameRate:], math.Float32bits(float32(fps)))
	le.PutUint32(sl[slSpatialBitrate:], uint32(bitrate))
	le.PutUint32(sl[slMaxSpatialBitrate:], uint32(bitrate*2))
	le.PutUint32(sl[slProfileIdc:], profileBaseline)
	le.PutUint32(sl[slLevelIdc:], 0) // let the encoder pick
	if threads > 1 {
		le.PutUint32(sl[slSliceMode:], sliceFixedNum)
		le.PutUint32(sl[slSliceNum:], uint32(threads))
	} else {
		le.PutUint32(sl[slSliceMode:], 0)
		le.PutUint32(sl[slSliceNum:], 1)
	}
	sl[slSignalType] = 1
	sl[slVideoFormat] = vfUndef
	sl[slFullRange] = 0
	sl[slColorDescription] = 1
	sl[slColorPrimaries] = bt709
	sl[slTransfer] = bt709
	sl[slColorMatrix] = bt709
	if r := e.call(vtInitializeExt, uintptr(unsafe.Pointer(&p[0]))); r != 0 {
		e.Close()
		return nil, fmt.Errorf("InitializeExt %dx%d failed (%d)", w, h, r)
	}

	e.yuv = make([]byte, w*h*3/2)
	e.pic = make([]byte, spSize)
	e.info = make([]byte, fbSize)
	pic := e.pic
	le.PutUint32(pic[spColorFormat:], videoFormatI420)
	le.PutUint32(pic[spStride:], uint32(w))
	le.PutUint32(pic[spStride+4:], uint32(w/2))
	le.PutUint32(pic[spStride+8:], uint32(w/2))
	le.PutUint32(pic[spWidth:], uint32(w))
	le.PutUint32(pic[spHeight:], uint32(h))
	return e, nil
}

func (e *H264Encoder) call(slot int, args ...uintptr) int32 {
	a := append([]uintptr{e.enc}, args...)
	return int32(dlCall(e.vt[slot], a...))
}

// SetRate changes the target bitrate (bit/s) and frame rate on the fly.
func (e *H264Encoder) SetRate(bitrate int, fps float64) {
	if bitrate != e.bitrate {
		e.bitrate = bitrate
		bi := [2]int32{spatialLayerAll, int32(bitrate)}
		e.call(vtSetOption, optBitrate, uintptr(unsafe.Pointer(&bi)))
		mx := [2]int32{spatialLayerAll, int32(bitrate * 2)}
		e.call(vtSetOption, optMaxBitrate, uintptr(unsafe.Pointer(&mx)))
	}
	if fps != e.fps {
		e.fps = fps
		f := float32(fps)
		e.call(vtSetOption, optFrameRate, uintptr(unsafe.Pointer(&f)))
	}
}

// Bitrate returns the current target bitrate.
func (e *H264Encoder) Bitrate() int { return e.bitrate }

// ForceKeyframe makes the next frame an IDR frame.
func (e *H264Encoder) ForceKeyframe() { e.call(vtForceIntra, 1) }

// Encode converts f (which must be W x H or larger) and encodes it. It returns the Annex B
// bitstream (nil when the rate control skipped the frame) and whether it is a keyframe.
func (e *H264Encoder) Encode(f *Frame, tsMillis int64) ([]byte, bool, error) {
	bgraToI420(f, e.yuv, e.W, e.H)
	le := binary.LittleEndian
	ySize := e.W * e.H
	le.PutUint64(e.pic[spData:], uint64(uintptr(unsafe.Pointer(&e.yuv[0]))))
	le.PutUint64(e.pic[spData+8:], uint64(uintptr(unsafe.Pointer(&e.yuv[ySize]))))
	le.PutUint64(e.pic[spData+16:], uint64(uintptr(unsafe.Pointer(&e.yuv[ySize+ySize/4]))))
	if tsMillis <= e.ts {
		tsMillis = e.ts + 1
	}
	e.ts = tsMillis
	le.PutUint64(e.pic[spTimestamp:], uint64(tsMillis))
	clear(e.info)
	r := e.call(vtEncodeFrame, uintptr(unsafe.Pointer(&e.pic[0])), uintptr(unsafe.Pointer(&e.info[0])))
	runtime.KeepAlive(e.yuv)
	if r != 0 {
		return nil, false, fmt.Errorf("EncodeFrame failed (%d)", r)
	}
	ft := le.Uint32(e.info[fbFrameType:])
	if ft == frameTypeSkip {
		return nil, false, nil
	}
	var out []byte
	layers := int(le.Uint32(e.info[fbLayerNum:]))
	for i := 0; i < layers && i < 128; i++ {
		lb := e.info[fbLayers+i*lbSize:]
		nals := int(le.Uint32(lb[lbNalCount:]))
		lens := uintptr(le.Uint64(lb[lbNalLengths:]))
		buf := uintptr(le.Uint64(lb[lbBsBuf:]))
		if nals <= 0 || lens == 0 || buf == 0 {
			continue
		}
		size := 0
		for n := 0; n < nals; n++ {
			size += int(*(*int32)(ptrFrom(lens + uintptr(n)*4)))
		}
		out = append(out, unsafe.Slice((*byte)(ptrFrom(buf)), size)...)
	}
	return out, ft == frameTypeIDR, nil
}

// Close releases the encoder.
func (e *H264Encoder) Close() {
	if e.enc != 0 {
		e.call(vtUninitialize)
		dlCall(e.lib.destroy, e.enc)
		e.enc = 0
	}
}

// ptrFrom converts an address that belongs to the C library into a pointer.
func ptrFrom(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// bgraToI420 converts the top-left w x h of f (BGRA) to I420, BT.709 limited range.
func bgraToI420(f *Frame, dst []byte, w, h int) {
	ySize := w * h
	cw := w / 2
	U := dst[ySize : ySize+ySize/4]
	V := dst[ySize+ySize/4:]
	parallelFor((h/2+15)/16, func(band int) {
		for cy := band * 16; cy < min(h/2, (band+1)*16); cy++ {
			r0 := f.Pix[(2*cy)*f.Stride:]
			r1 := f.Pix[(2*cy+1)*f.Stride:]
			y0 := dst[(2*cy)*w : (2*cy)*w+w]
			y1 := dst[(2*cy+1)*w : (2*cy+1)*w+w]
			u := U[cy*cw : cy*cw+cw]
			v := V[cy*cw : cy*cw+cw]
			for cx := 0; cx < cw; cx++ {
				i := cx * 8
				var sr, sg, sb int32
				for k, row := range [2][]byte{r0, r1} {
					yr := y0
					if k == 1 {
						yr = y1
					}
					for d := 0; d < 2; d++ {
						p := row[i+d*4 : i+d*4+3 : i+d*4+3]
						b, g, r := int32(p[0]), int32(p[1]), int32(p[2])
						yr[2*cx+d] = byte((47*r+157*g+16*b+128)>>8 + 16)
						sr += r
						sg += g
						sb += b
					}
				}
				u[cx] = byte((-26*sr-87*sg+112*sb+512)>>10 + 128)
				v[cx] = byte((112*sr-102*sg-10*sb+512)>>10 + 128)
			}
		}
	})
}
