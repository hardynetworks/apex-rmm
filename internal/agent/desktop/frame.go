// Package desktop implements Apex RMM's built-in remote desktop: screen
// capture, dirty-tile JPEG encoding and input injection for Windows, macOS and Linux (X11).
package desktop

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
)

// Frame is a BGRA (or BGRX) pixel buffer.
type Frame struct {
	W, H, Stride int
	Pix          []byte
}

// Display describes one monitor.
type Display struct {
	Name    string `json:"name"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Primary bool   `json:"primary"`
}

// Screen is implemented per OS.
type Screen interface {
	Displays() []Display
	Current() int
	Select(i int) error
	Capture() (*Frame, error)
	Move(x, y int)           // pixel coordinates within the captured area
	Button(b int, down bool) // 0 left, 1 middle, 2 right (DOM numbering)
	Wheel(dx, dy int)        // notches; dy > 0 scrolls down
	Key(code string, down bool)
	Type(text string)
	CtrlAltDel()
	Warning() string
	Close()
}

const tileSize = 64

// Encoder produces dirty-tile JPEG updates.
type Encoder struct {
	prev    []byte
	pw, ph  int
	Quality int
	scratch *image.RGBA
	buf     bytes.Buffer
}

// Reset forces the next frame to be sent in full.
func (e *Encoder) Reset() { e.prev = nil }

// Encode returns a binary message with all changed regions (nil if nothing changed).
//
// Layout: 0x01 | u16 count | count * (u16 x | u16 y | u16 w | u16 h | u32 len | jpeg)
func (e *Encoder) Encode(f *Frame) ([]byte, int) {
	full := e.prev == nil || e.pw != f.W || e.ph != f.H
	if full {
		e.prev = make([]byte, f.W*f.H*4)
		e.pw, e.ph = f.W, f.H
	}
	type rect struct{ x, y, w, h int }
	var rects []rect
	for ty := 0; ty < f.H; ty += tileSize {
		th := min(tileSize, f.H-ty)
		runStart := -1
		for tx := 0; tx < f.W; tx += tileSize {
			tw := min(tileSize, f.W-tx)
			changed := full || e.tileChanged(f, tx, ty, tw, th)
			if changed && runStart < 0 {
				runStart = tx
			}
			if !changed && runStart >= 0 {
				rects = append(rects, rect{runStart, ty, tx - runStart, th})
				runStart = -1
			}
		}
		if runStart >= 0 { // run reaches the right edge
			rects = append(rects, rect{runStart, ty, f.W - runStart, th})
		}
	}
	if len(rects) == 0 {
		return nil, 0
	}
	// Merge vertically adjacent rects with identical horizontal spans.
	merged := rects[:0:0]
	for _, r := range rects {
		done := false
		for i := range merged {
			m := &merged[i]
			if m.x == r.x && m.w == r.w && m.y+m.h == r.y {
				m.h += r.h
				done = true
				break
			}
		}
		if !done {
			merged = append(merged, r)
		}
	}
	out := bytes.NewBuffer(make([]byte, 0, 64<<10))
	out.WriteByte(1)
	_ = binary.Write(out, binary.BigEndian, uint16(len(merged)))
	for _, r := range merged {
		jb := e.encodeRect(f, r.x, r.y, r.w, r.h)
		_ = binary.Write(out, binary.BigEndian, [4]uint16{uint16(r.x), uint16(r.y), uint16(r.w), uint16(r.h)})
		_ = binary.Write(out, binary.BigEndian, uint32(len(jb)))
		out.Write(jb)
		// remember what the viewer now has
		for y := r.y; y < r.y+r.h; y++ {
			copy(e.prev[(y*f.W+r.x)*4:(y*f.W+r.x+r.w)*4], f.Pix[y*f.Stride+r.x*4:y*f.Stride+(r.x+r.w)*4])
		}
	}
	return out.Bytes(), len(merged)
}

func (e *Encoder) tileChanged(f *Frame, x, y, w, h int) bool {
	for row := y; row < y+h; row++ {
		a := f.Pix[row*f.Stride+x*4 : row*f.Stride+(x+w)*4]
		b := e.prev[(row*f.W+x)*4 : (row*f.W+x+w)*4]
		if !bytes.Equal(a, b) {
			return true
		}
	}
	return false
}

func (e *Encoder) encodeRect(f *Frame, x, y, w, h int) []byte {
	if e.scratch == nil || e.scratch.Rect.Dx() < w || e.scratch.Rect.Dy() < h {
		e.scratch = image.NewRGBA(image.Rect(0, 0, max(w, 256), max(h, 256)))
	}
	img := e.scratch.SubImage(image.Rect(0, 0, w, h)).(*image.RGBA)
	for row := 0; row < h; row++ {
		src := f.Pix[(y+row)*f.Stride+x*4 : (y+row)*f.Stride+(x+w)*4]
		dst := img.Pix[row*img.Stride : row*img.Stride+w*4]
		for i := 0; i < len(src); i += 4 {
			dst[i] = src[i+2]
			dst[i+1] = src[i+1]
			dst[i+2] = src[i]
			dst[i+3] = 255
		}
	}
	e.buf.Reset()
	q := e.Quality
	if q <= 0 {
		q = 70
	}
	_ = jpeg.Encode(&e.buf, img, &jpeg.Options{Quality: q})
	return append([]byte(nil), e.buf.Bytes()...)
}

// Scale downsamples a frame (nearest neighbour) by factor s in (0,1].
func Scale(f *Frame, s float64, dst *Frame) *Frame {
	if s >= 0.999 {
		return f
	}
	w, h := int(float64(f.W)*s), int(float64(f.H)*s)
	if w < 1 || h < 1 {
		return f
	}
	if dst.W != w || dst.H != h {
		*dst = Frame{W: w, H: h, Stride: w * 4, Pix: make([]byte, w*h*4)}
	}
	xs := make([]int, w)
	for x := range xs {
		xs[x] = (x * f.W / w) * 4
	}
	for y := 0; y < h; y++ {
		srow := f.Pix[(y*f.H/h)*f.Stride:]
		drow := dst.Pix[y*dst.Stride:]
		for x, sx := range xs {
			copy(drow[x*4:x*4+4], srow[sx:sx+4])
		}
	}
	return dst
}
