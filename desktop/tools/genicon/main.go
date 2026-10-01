// genicon draws the Apex RMM logo (a white "A" on a blue rounded square) and writes a
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
	blue  = color.RGBA{0x25, 0x63, 0xeb, 0xff}
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

type pt struct{ x, y float64 }

// render draws the logo, which is defined on a 32×32 grid (same as the web favicon).
func render(size int) *image.RGBA {
	s := float64(size) / 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// rounded square, radius 8
	r := vector.NewRasterizer(size, size)
	rr := 8 * s
	w := float64(size)
	k := 0.5523 * rr // cubic approximation of a quarter circle
	r.MoveTo(float32(rr), 0)
	r.LineTo(float32(w-rr), 0)
	r.CubeTo(float32(w-rr+k), 0, float32(w), float32(rr-k), float32(w), float32(rr))
	r.LineTo(float32(w), float32(w-rr))
	r.CubeTo(float32(w), float32(w-rr+k), float32(w-rr+k), float32(w), float32(w-rr), float32(w))
	r.LineTo(float32(rr), float32(w))
	r.CubeTo(float32(rr-k), float32(w), 0, float32(w-rr+k), 0, float32(w-rr))
	r.LineTo(0, float32(rr))
	r.CubeTo(0, float32(rr-k), float32(rr-k), 0, float32(rr), 0)
	r.ClosePath()
	r.Draw(img, img.Bounds(), image.NewUniform(blue), image.Point{})

	// the "A": two legs and a crossbar, stroke width 3.2, round caps and joins
	strokes := [][2]pt{
		{{8.5, 24}, {16, 8}},
		{{16, 8}, {23.5, 24}},
		{{11.6, 18.5}, {20.4, 18.5}},
	}
	half := 1.6 * s
	mask := image.NewAlpha(img.Bounds())
	for _, st := range strokes {
		a, b := pt{st[0].x * s, st[0].y * s}, pt{st[1].x * s, st[1].y * s}
		// each piece is drawn on its own rasterizer and merged with "max", so overlaps never cancel
		addShape(mask, size, func(r *vector.Rasterizer) { segment(r, a, b, half) })
		addShape(mask, size, func(r *vector.Rasterizer) { circle(r, a, half) })
		addShape(mask, size, func(r *vector.Rasterizer) { circle(r, b, half) })
	}
	draw.DrawMask(img, img.Bounds(), image.NewUniform(white), image.Point{}, mask, image.Point{}, draw.Over)
	return img
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
