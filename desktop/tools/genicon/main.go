// genicon draws the Apex RMM logo (a gradient "A" with a pulse crossbar on a dark rounded square) and writes a
// multi-size Windows .ico file, so the repository doesn't need binary image files.
//
//	go run ./tools/genicon -out build/icon.ico [-png build/icon.png]
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/vector"
)

var (
	cyan   = color.RGBA{0x22, 0xd3, 0xee, 0xff}
	blue   = color.RGBA{0x3b, 0x82, 0xf6, 0xff}
	purple = color.RGBA{0xa8, 0x55, 0xf7, 0xff}
	bgTop  = color.RGBA{0x16, 0x20, 0x3a, 0xff}
	bgBot  = color.RGBA{0x06, 0x0a, 0x14, 0xff}
	pulse  = color.RGBA{0xe0, 0xf2, 0xfe, 0xff}
	white  = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

type pt struct{ x, y float64 }

// gradient is an image whose colour runs through stops as t goes from 0 to 1;
// at maps a pixel position to t.
type gradient struct {
	stops []color.RGBA
	at    func(x, y int) float64
}

func (g gradient) ColorModel() color.Model { return color.RGBAModel }
func (g gradient) Bounds() image.Rectangle { return image.Rect(-1e9, -1e9, 1e9, 1e9) }
func (g gradient) At(x, y int) color.Color {
	t := math.Max(0, math.Min(1, g.at(x, y))) * float64(len(g.stops)-1)
	i := int(t)
	if i >= len(g.stops)-1 {
		return g.stops[len(g.stops)-1]
	}
	f := t - float64(i)
	a, b := g.stops[i], g.stops[i+1]
	mix := func(p, q uint8) uint8 { return uint8(float64(p) + (float64(q)-float64(p))*f + 0.5) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xff}
}

// render draws the logo, which is defined on a 32×32 grid (same as the web favicon):
// a dark rounded square with a gradient edge, a gradient "A" whose crossbar is a
// monitoring pulse, and a glowing dot on the apex.
func render(size int) *image.RGBA {
	s := float64(size) / 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	// cyan → blue → purple from bottom-left to top-right; dark navy top to bottom
	accent := gradient{[]color.RGBA{cyan, blue, purple}, func(x, y int) float64 {
		return (float64(x) + float64(size-y)) / float64(2*size)
	}}
	bg := gradient{[]color.RGBA{bgTop, bgBot}, func(x, y int) float64 { return float64(y) / float64(size) }}

	// paint fills the union of shapes with src at the given opacity
	paint := func(src image.Image, opacity float64, shapes ...func(*vector.Rasterizer)) {
		mask := image.NewAlpha(img.Bounds())
		for _, f := range shapes {
			addShape(mask, size, f)
		}
		for i, v := range mask.Pix {
			mask.Pix[i] = uint8(float64(v) * opacity)
		}
		draw.DrawMask(img, img.Bounds(), src, image.Point{}, mask, image.Point{}, draw.Over)
	}

	// background with a 1-unit gradient edge at 60%: the edge colour fills the whole
	// square, then the dark fill is drawn inset by one unit
	paint(bg, 1, func(r *vector.Rasterizer) { roundRect(r, 0, 32*s, 8*s) })
	paint(accent, 0.6, func(r *vector.Rasterizer) { roundRect(r, 0, 32*s, 8*s) })
	paint(bg, 1, func(r *vector.Rasterizer) { roundRect(r, 1*s, 30*s, 7*s) })

	// the "A": two legs, stroke width 3.2, round caps and joins
	paint(accent, 1, polyline([]pt{{7, 25}, {16, 7.5}, {25, 25}}, s, 1.6)...)
	// the pulse crossbar, stroke width 1.6
	paint(image.NewUniform(pulse), 1, polyline([]pt{{10.3, 18.5}, {13, 18.5}, {14.5, 15.5}, {16.6, 21.5}, {18.2, 18.5}, {21.7, 18.5}}, s, 0.8)...)
	// glowing dot on the apex
	apex := pt{16 * s, 7.5 * s}
	paint(image.NewUniform(cyan), 0.3, func(r *vector.Rasterizer) { circle(r, apex, 3.6*s) })
	paint(image.NewUniform(white), 1, func(r *vector.Rasterizer) { circle(r, apex, 1.9*s) })
	return img
}

// polyline returns the shapes for a stroked line through pts (on the 32×32 grid)
// with round caps and joins; half is half the stroke width.
func polyline(pts []pt, s, half float64) []func(*vector.Rasterizer) {
	var shapes []func(*vector.Rasterizer)
	for i, p := range pts {
		c := pt{p.x * s, p.y * s}
		shapes = append(shapes, func(r *vector.Rasterizer) { circle(r, c, half*s) })
		if i > 0 {
			a := pt{pts[i-1].x * s, pts[i-1].y * s}
			shapes = append(shapes, func(r *vector.Rasterizer) { segment(r, a, c, half*s) })
		}
	}
	return shapes
}

// roundRect adds a square of side w at (o, o) with corner radius rr.
func roundRect(r *vector.Rasterizer, o, w, rr float64) {
	k := 0.5523 * rr // cubic approximation of a quarter circle
	e := o + w
	r.MoveTo(float32(o+rr), float32(o))
	r.LineTo(float32(e-rr), float32(o))
	r.CubeTo(float32(e-rr+k), float32(o), float32(e), float32(o+rr-k), float32(e), float32(o+rr))
	r.LineTo(float32(e), float32(e-rr))
	r.CubeTo(float32(e), float32(e-rr+k), float32(e-rr+k), float32(e), float32(e-rr), float32(e))
	r.LineTo(float32(o+rr), float32(e))
	r.CubeTo(float32(o+rr-k), float32(e), float32(o), float32(e-rr+k), float32(o), float32(e-rr))
	r.LineTo(float32(o), float32(o+rr))
	r.CubeTo(float32(o), float32(o+rr-k), float32(o+rr-k), float32(o), float32(o+rr), float32(o))
	r.ClosePath()
}

func addShape(dst *image.Alpha, size int, f func(*vector.Rasterizer)) {
	r := vector.NewRasterizer(size, size)
	f(r)
	tmp := image.NewAlpha(dst.Bounds())
	r.Draw(tmp, tmp.Bounds(), image.Opaque, image.Point{})
	for i, v := range tmp.Pix {
		if v > dst.Pix[i] {
			dst.Pix[i] = v
		}
	}
}

func segment(r *vector.Rasterizer, a, b pt, half float64) {
	dx, dy := b.x-a.x, b.y-a.y
	l := math.Hypot(dx, dy)
	nx, ny := -dy/l*half, dx/l*half
	r.MoveTo(float32(a.x+nx), float32(a.y+ny))
	r.LineTo(float32(b.x+nx), float32(b.y+ny))
	r.LineTo(float32(b.x-nx), float32(b.y-ny))
	r.LineTo(float32(a.x-nx), float32(a.y-ny))
	r.ClosePath()
}

func circle(r *vector.Rasterizer, c pt, rad float64) {
	const n = 32
	for i := 0; i <= n; i++ {
		t := 2 * math.Pi * float64(i) / n
		x, y := float32(c.x+rad*math.Cos(t)), float32(c.y+rad*math.Sin(t))
		if i == 0 {
			r.MoveTo(x, y)
		} else {
			r.LineTo(x, y)
		}
	}
	r.ClosePath()
}

// dib encodes an image as a 32-bit BMP icon entry (bottom-up BGRA plus an empty AND mask).
func dib(img *image.RGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	var b bytes.Buffer
	hdr := struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPels, YPels           int32
		ClrUsed, ClrImportant  uint32
	}{40, int32(w), int32(h * 2), 1, 32, 0, 0, 0, 0, 0, 0}
	_ = binary.Write(&b, binary.LittleEndian, hdr)
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.RGBAAt(x, y) // premultiplied in image.RGBA; un-premultiply for the icon
			if c.A != 0 && c.A != 255 {
				c.R = uint8(uint32(c.R) * 255 / uint32(c.A))
				c.G = uint8(uint32(c.G) * 255 / uint32(c.A))
				c.B = uint8(uint32(c.B) * 255 / uint32(c.A))
			}
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	maskRow := ((w + 31) / 32) * 4
	b.Write(make([]byte, maskRow*h))
	return b.Bytes()
}

func main() {
	out := flag.String("out", "build/icon.ico", "output .ico file")
	pngOut := flag.String("png", "", "also write a 256px PNG preview here")
	flag.Parse()

	sizes := []int{16, 20, 24, 32, 40, 48, 64, 96, 128, 256}
	var images [][]byte
	for _, sz := range sizes {
		img := render(sz)
		if sz == 256 {
			var p bytes.Buffer
			if err := png.Encode(&p, img); err != nil {
				log.Fatal(err)
			}
			images = append(images, p.Bytes()) // Vista+ icons store the 256px image as PNG
			if *pngOut != "" {
				_ = os.MkdirAll(filepath.Dir(*pngOut), 0o755)
				if err := os.WriteFile(*pngOut, p.Bytes(), 0o644); err != nil {
					log.Fatal(err)
				}
			}
		} else {
			images = append(images, dib(img))
		}
	}

	var ico bytes.Buffer
	_ = binary.Write(&ico, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, sz := range sizes {
		dim := uint8(sz)
		if sz == 256 {
			dim = 0
		}
		_ = binary.Write(&ico, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BitCount       uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(images[i])), uint32(offset)})
		offset += len(images[i])
	}
	for _, im := range images {
		ico.Write(im)
	}
	_ = os.MkdirAll(filepath.Dir(*out), 0o755)
	if err := os.WriteFile(*out, ico.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d sizes)", *out, len(sizes))
}
