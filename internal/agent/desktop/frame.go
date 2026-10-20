// Package desktop implements Apex RMM's built-in remote desktop: screen
// capture, dirty-tile JPEG encoding and input injection for Windows, macOS and Linux (X11).
package desktop

import (
	"bytes"
	"encoding/binary"
	"hash/maphash"
	"image"
	"image/jpeg"
	"runtime"
	"sync"
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
	Move(x, y int)                      // pixel coordinates within the captured area
	Global(x, y int) (float64, float64) // the same point in the OS's desktop coordinates
	Button(b int, down bool)            // 0 left, 1 middle, 2 right (DOM numbering)
	Wheel(dx, dy int)                   // notches; dy > 0 scrolls down
	Key(code string, down bool)
	Type(text string)
	CtrlAltDel()
	Warning() string
	Close()
}

const (
	tileSize = 64
	// Merged rectangles are at most this many tiles tall, so a big update is split into bands
	// that encode in parallel and decode progressively in the browser.
	maxBandTiles = 2
)

// Tile classes.
const (
	clsSame   uint32 = 0 // unchanged
	clsJPEG   uint32 = 1 // changed: JPEG at this frame's quality
	clsRefine uint32 = 2 // unchanged, but last sent at motion quality: re-send at full quality
	clsSolid  uint32 = 3 // changed and a single colour (protocol 2); colour in bits 8..31
	clsCopy   uint32 = 4 // changed, but the viewer already has it dy rows away (scrolling); candidate index in bits 8..
)

// Encoder produces dirty-tile updates.
//
// Protocol 1 (all viewers):  0x01 | u16 count | count * (u16 x | u16 y | u16 w | u16 h | u32 len | jpeg)
// Protocol 2 (newer viewers): 0x02 | u16 count | count * (u8 kind | u16 x | u16 y | u16 w | u16 h | payload)
// where kind 0 is a JPEG (payload u32 len | jpeg), kind 1 a solid fill (payload r | g | b) and
// kind 2 a copy of another area of the previous frame (payload u16 srcX | u16 srcY). Copies in a
// message all read the screen as it was before the message.
//
// Scrolling is detected by matching rows of changed tiles against the previous frame, so a
// scrolled page costs only the newly exposed strip instead of the whole window.
//
// With Adaptive set (protocol 2), large updates such as scrolling or dragging a window are sent at
// a lower "motion" quality, and those tiles are re-sent at full quality once they stop changing.
type Encoder struct {
	Quality  int
	Proto    int
	Adaptive bool

	prev   []byte
	pw, ph int
	gw, gh int      // tile grid
	cls    []uint32 // per tile, this frame
	lowq   []bool   // per tile: last sent at motion quality
	stable []uint8  // per tile: frames since it last changed (saturating)
	bufs   sync.Pool
	seed   maphash.Seed
	dys    []int // scroll offsets found in this frame
}

// Reset forces the next frame to be sent in full.
func (e *Encoder) Reset() { e.prev = nil }

type rect struct {
	x, y, w, h int
	cls        uint32
	data       []byte
}

// Encode returns a binary message with all changed regions (nil if nothing changed).
func (e *Encoder) Encode(f *Frame) ([]byte, int) {
	full := e.prev == nil || e.pw != f.W || e.ph != f.H
	if full {
		if len(e.prev) != f.W*f.H*4 {
			e.prev = make([]byte, f.W*f.H*4)
		}
		e.pw, e.ph = f.W, f.H
		e.gw, e.gh = (f.W+tileSize-1)/tileSize, (f.H+tileSize-1)/tileSize
		n := e.gw * e.gh
		e.cls, e.lowq, e.stable = make([]uint32, n), make([]bool, n), make([]uint8, n)
	}
	proto2 := e.Proto >= 2

	// 1. Classify tiles (in parallel by tile row).
	changed := make([]int, e.gh)
	parallelFor(e.gh, func(ty int) {
		y0 := ty * tileSize
		th := min(tileSize, f.H-y0)
		for tx := 0; tx < e.gw; tx++ {
			x0 := tx * tileSize
			tw := min(tileSize, f.W-x0)
			i := ty*e.gw + tx
			if !full && !e.tileChanged(f, x0, y0, tw, th) {
				e.cls[i] = clsSame
				if e.stable[i] < 255 {
					e.stable[i]++
				}
				continue
			}
			e.stable[i] = 0
			changed[ty]++
			e.cls[i] = clsJPEG
			if proto2 {
				if c, ok := solidColor(f, x0, y0, tw, th); ok {
					e.cls[i] = clsSolid | c<<8
				}
			}
		}
	})
	nChanged := 0
	for _, c := range changed {
		nChanged += c
	}
	var lowqBefore []bool
	if proto2 && !full && nChanged >= 6 {
		if e.findScroll(f) {
			lowqBefore = append([]bool(nil), e.lowq...)
			nChanged = 0
			for _, c := range e.cls {
				if c == clsJPEG {
					nChanged++
				}
			}
		}
	}

	// 2. Pick this frame's quality; schedule refinement of tiles that have settled.
	q := e.Quality
	if q <= 0 {
		q = 70
	}
	frameQ, motion := q, false
	if proto2 && e.Adaptive && !full && nChanged*8 >= len(e.cls) { // ≥ 1/8 of the screen changed
		motion = true
		frameQ = max(20, q*55/100)
	}
	if proto2 && e.Adaptive {
		for i, c := range e.cls {
			if c == clsSame && e.lowq[i] && e.stable[i] >= 2 {
				e.cls[i] = clsRefine
			}
		}
	}
	if !motion { // refine and fresh tiles share one quality, so let them merge
		for i, c := range e.cls {
			if c == clsRefine {
				e.cls[i] = clsJPEG
			}
		}
	}

	// 3. Merge tiles of the same class into rectangles: horizontal runs, then vertically
	// adjacent runs with the same span (up to maxBandTiles tall).
	var rects []rect
	for ty := 0; ty < e.gh; ty++ {
		y0 := ty * tileSize
		th := min(tileSize, f.H-y0)
		for tx := 0; tx < e.gw; {
			c := e.cls[ty*e.gw+tx]
			if c == clsSame {
				tx++
				continue
			}
			end := tx + 1
			for end < e.gw && e.cls[ty*e.gw+end] == c {
				end++
			}
			r := rect{x: tx * tileSize, y: y0, w: min(end*tileSize, f.W) - tx*tileSize, h: th, cls: c}
			merged := false
			for i := len(rects) - 1; i >= 0; i-- {
				m := &rects[i]
				if m.y+m.h == y0 && m.x == r.x && m.w == r.w && m.cls == c && m.h < maxBandTiles*tileSize {
					m.h += th
					merged = true
					break
				}
			}
			if !merged {
				rects = append(rects, r)
			}
			tx = end
		}
	}
	if len(rects) == 0 {
		return nil, 0
	}

	// 4. Encode the rectangles in parallel and remember what the viewer now has.
	parallelFor(len(rects), func(i int) {
		r := &rects[i]
		switch r.cls & 0xff {
		case clsSolid:
			c := r.cls >> 8
			r.data = []byte{byte(c >> 16), byte(c >> 8), byte(c)} // r, g, b
		case clsRefine:
			r.data = e.encodeRect(f, r.x, r.y, r.w, r.h, q)
		case clsCopy:
			sy := r.y - e.dys[r.cls>>8]
			r.data = []byte{byte(r.x >> 8), byte(r.x), byte(sy >> 8), byte(sy)}
		default:
			r.data = e.encodeRect(f, r.x, r.y, r.w, r.h, frameQ)
		}
		for y := r.y; y < r.y+r.h; y++ {
			copy(e.prev[(y*f.W+r.x)*4:(y*f.W+r.x+r.w)*4], f.Pix[y*f.Stride+r.x*4:y*f.Stride+(r.x+r.w)*4])
		}
		low := motion && r.cls == clsJPEG
		isCopy := r.cls&0xff == clsCopy
		for ty := r.y / tileSize; ty < (r.y+r.h+tileSize-1)/tileSize; ty++ {
			for tx := r.x / tileSize; tx < (r.x+r.w+tileSize-1)/tileSize; tx++ {
				if isCopy { // a copy is as sharp as the area it came from
					sy := ty*tileSize - e.dys[r.cls>>8]
					a, b := clampInt(sy/tileSize, 0, e.gh-1), clampInt((sy+tileSize-1)/tileSize, 0, e.gh-1)
					low = lowqBefore[a*e.gw+tx] || lowqBefore[b*e.gw+tx]
				}
				e.lowq[ty*e.gw+tx] = low
			}
		}
	})

	size := 3
	for _, r := range rects {
		size += 14 + len(r.data)
	}
	out := bytes.NewBuffer(make([]byte, 0, size))
	if proto2 {
		out.WriteByte(2)
	} else {
		out.WriteByte(1)
	}
	_ = binary.Write(out, binary.BigEndian, uint16(len(rects)))
	for _, r := range rects {
		kind := byte(0)
		switch r.cls & 0xff {
		case clsSolid:
			kind = 1
		case clsCopy:
			kind = 2
		}
		if proto2 {
			out.WriteByte(kind)
		}
		_ = binary.Write(out, binary.BigEndian, [4]uint16{uint16(r.x), uint16(r.y), uint16(r.w), uint16(r.h)})
		if kind == 0 {
			_ = binary.Write(out, binary.BigEndian, uint32(len(r.data)))
		}
		out.Write(r.data)
	}
	return out.Bytes(), len(rects)
}

// findScroll looks for vertical scrolling: changed tiles whose content the viewer already has
// some rows higher or lower. Matching tiles are reclassified as copies. Reports whether any were.
func (e *Encoder) findScroll(f *Frame) bool {
	if e.seed == (maphash.Seed{}) {
		e.seed = maphash.MakeSeed()
	}
	// Index the previous frame: per tile column, the hash of every row segment that occurs once.
	type colIndex map[uint64]int32
	cols := make([]colIndex, e.gw)
	need := make([]bool, e.gw)
	for i, c := range e.cls {
		if c == clsJPEG {
			need[i%e.gw] = true
		}
	}
	parallelFor(e.gw, func(tx int) {
		if !need[tx] {
			return
		}
		x0, tw := tx*tileSize, min(tileSize, f.W-tx*tileSize)
		idx := make(colIndex, f.H)
		for y := 0; y < f.H; y++ {
			seg := e.prev[(y*f.W+x0)*4 : (y*f.W+x0+tw)*4]
			h := maphash.Bytes(e.seed, seg)
			if _, dup := idx[h]; dup {
				idx[h] = -1
			} else {
				idx[h] = int32(y)
			}
		}
		cols[tx] = idx
	})
	// Vote: sample a few rows of each changed tile and see where they were before.
	votes := map[int]int{}
	for i, c := range e.cls {
		if c != clsJPEG {
			continue
		}
		tx, ty := i%e.gw, i/e.gw
		x0, y0 := tx*tileSize, ty*tileSize
		tw, th := min(tileSize, f.W-x0), min(tileSize, f.H-y0)
		for _, r := range [...]int{5, 21, 37, 53} {
			if r >= th {
				break
			}
			y := y0 + r
			seg := f.Pix[y*f.Stride+x0*4 : y*f.Stride+(x0+tw)*4]
			if py, ok := cols[tx][maphash.Bytes(e.seed, seg)]; ok && py >= 0 && int(py) != y {
				votes[y-int(py)]++
			}
		}
	}
	e.dys = e.dys[:0]
	for len(e.dys) < 3 {
		best, n := 0, 3 // need a few agreeing rows
		for dy, v := range votes {
			if v > n || (v == n && best != 0 && absInt(dy) < absInt(best)) {
				best, n = dy, v
			}
		}
		if best == 0 {
			break
		}
		e.dys = append(e.dys, best)
		delete(votes, best)
	}
	if len(e.dys) == 0 {
		return false
	}
	// Reclassify tiles that match the previous frame exactly at one of those offsets.
	found := make([]bool, e.gh)
	parallelFor(e.gh, func(ty int) {
		y0 := ty * tileSize
		th := min(tileSize, f.H-y0)
		for tx := 0; tx < e.gw; tx++ {
			i := ty*e.gw + tx
			if e.cls[i] != clsJPEG {
				continue
			}
			x0, tw := tx*tileSize, min(tileSize, f.W-tx*tileSize)
			for k, dy := range e.dys {
				sy := y0 - dy
				if sy < 0 || sy+th > f.H {
					continue
				}
				if e.matchesPrev(f, x0, y0, tw, th, sy) {
					e.cls[i] = clsCopy | uint32(k)<<8
					found[ty] = true
					break
				}
			}
		}
	})
	for _, ok := range found {
		if ok {
			return true
		}
	}
	return false
}

// matchesPrev reports whether the frame's area at (x,y) equals the previous frame's at (x,sy).
func (e *Encoder) matchesPrev(f *Frame, x, y, w, h, sy int) bool {
	for row := 0; row < h; row++ {
		a := f.Pix[(y+row)*f.Stride+x*4 : (y+row)*f.Stride+(x+w)*4]
		b := e.prev[((sy+row)*f.W+x)*4 : ((sy+row)*f.W+x+w)*4]
		if !bytes.Equal(a, b) {
			return false
		}
	}
	return true
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func clampInt(v, lo, hi int) int { return max(lo, min(hi, v)) }

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

// solidColor reports whether a tile is a single colour, returned as 0xRRGGBB.
func solidColor(f *Frame, x, y, w, h int) (uint32, bool) {
	const mask = 0x00ffffff00ffffff // ignore the alpha/padding byte
	p := binary.LittleEndian.Uint32(f.Pix[y*f.Stride+x*4:]) & 0x00ffffff
	want := uint64(p) | uint64(p)<<32
	for row := y; row < y+h; row++ {
		s := f.Pix[row*f.Stride+x*4 : row*f.Stride+(x+w)*4]
		i := 0
		for ; i+8 <= len(s); i += 8 {
			if binary.LittleEndian.Uint64(s[i:])&mask != want {
				return 0, false
			}
		}
		if i < len(s) && binary.LittleEndian.Uint32(s[i:])&0x00ffffff != p {
			return 0, false
		}
	}
	return p, true // BGRX little-endian reads as 0x00RRGGBB
}

type encBuf struct {
	img *image.RGBA
	out bytes.Buffer
}

func (e *Encoder) encodeRect(f *Frame, x, y, w, h, q int) []byte {
	eb, _ := e.bufs.Get().(*encBuf)
	if eb == nil {
		eb = &encBuf{}
	}
	defer e.bufs.Put(eb)
	if eb.img == nil || eb.img.Rect.Dx() < w || eb.img.Rect.Dy() < h {
		eb.img = image.NewRGBA(image.Rect(0, 0, max(w, 256), max(h, maxBandTiles*tileSize)))
	}
	img := eb.img.SubImage(image.Rect(0, 0, w, h)).(*image.RGBA)
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
	eb.out.Reset()
	_ = jpeg.Encode(&eb.out, img, &jpeg.Options{Quality: q})
	return append([]byte(nil), eb.out.Bytes()...)
}

// parallelFor runs fn(0..n-1) across the available CPUs.
func parallelFor(n int, fn func(i int)) {
	workers := min(n, runtime.GOMAXPROCS(0))
	if workers <= 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	next := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// Scale downsamples a frame by factor s in (0,1], averaging 2x2 source pixels so text stays
// legible (plain nearest-neighbour drops whole pixel columns).
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
	xs2 := make([]int, w)
	for x := range xs {
		sx := x * f.W / w
		xs[x] = sx * 4
		xs2[x] = min(sx+1, f.W-1) * 4
	}
	const band = 32
	parallelFor((h+band-1)/band, func(b int) {
		for y := b * band; y < min(h, (b+1)*band); y++ {
			sy := y * f.H / h
			r0 := f.Pix[sy*f.Stride:]
			r1 := f.Pix[min(sy+1, f.H-1)*f.Stride:]
			drow := dst.Pix[y*dst.Stride:]
			for x := range xs {
				a, c := xs[x], xs2[x]
				d := drow[x*4 : x*4+4 : x*4+4]
				d[0] = byte((uint(r0[a]) + uint(r0[c]) + uint(r1[a]) + uint(r1[c]) + 2) >> 2)
				d[1] = byte((uint(r0[a+1]) + uint(r0[c+1]) + uint(r1[a+1]) + uint(r1[c+1]) + 2) >> 2)
				d[2] = byte((uint(r0[a+2]) + uint(r0[c+2]) + uint(r1[a+2]) + uint(r1[c+2]) + 2) >> 2)
				d[3] = 255
			}
		}
	})
	return dst
}
