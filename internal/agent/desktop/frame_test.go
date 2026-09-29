package desktop

import (
	"bytes"
	"encoding/binary"
	"image/jpeg"
	"testing"
)

func solid(w, h int, b, g, r byte) *Frame {
	f := &Frame{W: w, H: h, Stride: w * 4, Pix: make([]byte, w*h*4)}
	for i := 0; i < len(f.Pix); i += 4 {
		f.Pix[i], f.Pix[i+1], f.Pix[i+2], f.Pix[i+3] = b, g, r, 255
	}
	return f
}

func TestEncoderDirtyTiles(t *testing.T) {
	e := &Encoder{Quality: 70}
	f := solid(300, 200, 10, 20, 30)
	msg, n := e.Encode(f)
	if n == 0 || msg[0] != 1 {
		t.Fatalf("first frame should be sent in full, got %d rects", n)
	}
	if _, n := e.Encode(f); n != 0 {
		t.Fatalf("unchanged frame produced %d rects", n)
	}
	// change one pixel inside tile (1,1)
	f.Pix[(70*f.W+70)*4] = 255
	msg, n = e.Encode(f)
	if n != 1 {
		t.Fatalf("expected 1 dirty rect, got %d", n)
	}
	x := binary.BigEndian.Uint16(msg[3:])
	y := binary.BigEndian.Uint16(msg[5:])
	w := binary.BigEndian.Uint16(msg[7:])
	l := binary.BigEndian.Uint32(msg[11:])
	if x != 64 || y != 64 || w != 64 {
		t.Fatalf("unexpected rect %d,%d w=%d", x, y, w)
	}
	img, err := jpeg.Decode(bytes.NewReader(msg[15 : 15+l]))
	if err != nil || img.Bounds().Dx() != 64 {
		t.Fatalf("tile is not a valid 64px JPEG: %v", err)
	}
}

func TestScale(t *testing.T) {
	var dst Frame
	s := Scale(solid(400, 200, 1, 2, 3), 0.5, &dst)
	if s.W != 200 || s.H != 100 || s.Pix[2] != 3 {
		t.Fatalf("bad scale %dx%d", s.W, s.H)
	}
}

func TestKeymaps(t *testing.T) {
	if evdev["KeyA"] != 30 || usASCII['A'] != [2]int{30, 1} || macKeys["KeyA"] != 0 {
		t.Fatal("keymap mismatch")
	}
}
