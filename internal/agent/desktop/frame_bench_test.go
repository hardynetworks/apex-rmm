package desktop

import (
	"image"
	"image/draw"
	_ "image/png"
	"os"
	"testing"
)

// Benchmarks use real 1920x1080 screenshots when APEX_BENCH_DIR points at bench1.png / bench2.png,
// and a synthetic UI-like frame otherwise.
func benchFrame(tb testing.TB, name string, seed byte) *Frame {
	if dir := os.Getenv("APEX_BENCH_DIR"); dir != "" {
		if fh, err := os.Open(dir + "/" + name); err == nil {
			defer fh.Close()
			if img, _, err := image.Decode(fh); err == nil {
				b := img.Bounds()
				rgba := image.NewRGBA(b)
				draw.Draw(rgba, b, img, b.Min, draw.Src)
				f := &Frame{W: b.Dx(), H: b.Dy(), Stride: b.Dx() * 4, Pix: make([]byte, b.Dx()*b.Dy()*4)}
				for i := 0; i < len(f.Pix); i += 4 {
					f.Pix[i], f.Pix[i+1], f.Pix[i+2], f.Pix[i+3] = rgba.Pix[i+2], rgba.Pix[i+1], rgba.Pix[i], 255
				}
				return f
			}
		}
	}
	f := solid(1920, 1080, 245, 246, 248)
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			if (x/7+y/13+int(seed))%9 == 0 || x < 240 {
				i := y*f.Stride + x*4
				f.Pix[i], f.Pix[i+1], f.Pix[i+2] = byte(x)+seed, byte(y), byte(x^y)
			}
		}
	}
	return f
}

func BenchmarkEncodeFull(b *testing.B) {
	b.Run("v1", func(b *testing.B) { benchFull(b, &Encoder{Quality: 60}) })
	b.Run("v2", func(b *testing.B) { benchFull(b, &Encoder{Quality: 60, Proto: 2, Adaptive: true}) })
}

func benchFull(b *testing.B, e *Encoder) {
	f := benchFrame(b, "bench1.png", 0)
	var bytes int
	for i := 0; i < b.N; i++ {
		e.Reset()
		msg, _ := e.Encode(f)
		bytes = len(msg)
	}
	b.ReportMetric(float64(bytes)/1024, "KB/frame")
}

func BenchmarkEncodePageSwitch(b *testing.B) {
	b.Run("v1", func(b *testing.B) { benchSwitch(b, &Encoder{Quality: 60}) })
	b.Run("v2", func(b *testing.B) { benchSwitch(b, &Encoder{Quality: 60, Proto: 2, Adaptive: true}) })
}

func benchSwitch(b *testing.B, e *Encoder) {
	f1, f2 := benchFrame(b, "bench1.png", 0), benchFrame(b, "bench2.png", 3)
	e.Encode(f1)
	var bytes int
	for i := 0; i < b.N; i++ {
		f := f2
		if i%2 == 1 {
			f = f1
		}
		msg, _ := e.Encode(f)
		bytes = len(msg)
	}
	b.ReportMetric(float64(bytes)/1024, "KB/frame")
}

func BenchmarkEncodeSmallChange(b *testing.B) {
	f := benchFrame(b, "bench1.png", 0)
	e := &Encoder{Quality: 60}
	e.Encode(f)
	for i := 0; i < b.N; i++ {
		p := (500*f.W + 500) * 4
		f.Pix[p] ^= 0xff // a blinking caret
		e.Encode(f)
	}
}

func BenchmarkScale(b *testing.B) {
	f := benchFrame(b, "bench1.png", 0)
	var dst Frame
	for i := 0; i < b.N; i++ {
		Scale(f, 0.75, &dst)
	}
}
