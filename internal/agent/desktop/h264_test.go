package desktop

import (
	"os"
	"testing"
	"time"
)

// Set APEX_OPENH264 to an OpenH264 2.6 library to run these.
func testEncoder(t testing.TB, w, h, bitrate int) *H264Encoder {
	path := os.Getenv("APEX_OPENH264")
	if path == "" {
		t.Skip("APEX_OPENH264 not set")
	}
	lib, err := loadOpenH264(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewH264Encoder(lib, w, h, bitrate, 60)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestH264Encode(t *testing.T) {
	e := testEncoder(t, 1920, 1080, 4_000_000)
	defer e.Close()
	var all []byte
	keys := 0
	for i := 0; i < 60; i++ {
		f := page(1920, 1080, i*6) // scrolling document
		b, key, err := e.Encode(f, int64(i*16))
		if err != nil {
			t.Fatal(err)
		}
		if key {
			keys++
		}
		all = append(all, b...)
	}
	if keys != 1 {
		t.Fatalf("expected exactly one keyframe, got %d", keys)
	}
	if out := os.Getenv("APEX_H264_OUT"); out != "" {
		_ = os.WriteFile(out, all, 0o644)
	}
	t.Logf("60 frames, %d KB total", len(all)/1024)
	e.ForceKeyframe()
	if _, key, _ := e.Encode(page(1920, 1080, 999), 2000); !key {
		t.Fatal("ForceKeyframe didn't produce an IDR")
	}
}

func BenchmarkH264Encode1080p(b *testing.B) {
	e := testEncoder(b, 1920, 1080, 6_000_000)
	defer e.Close()
	frames := []*Frame{page(1920, 1080, 0), page(1920, 1080, 6), page(1920, 1080, 12)}
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if _, _, err := e.Encode(frames[i%3], int64(i*16)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "fps")
}

func BenchmarkI420(b *testing.B) {
	f := page(1920, 1080, 0)
	dst := make([]byte, 1920*1080*3/2)
	for i := 0; i < b.N; i++ {
		bgraToI420(f, dst, 1920, 1080)
	}
}
