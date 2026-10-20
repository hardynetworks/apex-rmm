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

// parse2 decodes a protocol-2 message into (kind, x, y, w, h) tuples.
func parse2(t *testing.T, msg []byte) [][5]int {
	t.Helper()
	if msg[0] != 2 {
		t.Fatalf("not a protocol 2 message: %d", msg[0])
	}
	n := int(binary.BigEndian.Uint16(msg[1:]))
	off := 3
	var out [][5]int
	for i := 0; i < n; i++ {
		k := int(msg[off])
		x, y := int(binary.BigEndian.Uint16(msg[off+1:])), int(binary.BigEndian.Uint16(msg[off+3:]))
		w, h := int(binary.BigEndian.Uint16(msg[off+5:])), int(binary.BigEndian.Uint16(msg[off+7:]))
		off += 9
		if k == 0 {
			l := int(binary.BigEndian.Uint32(msg[off:]))
			if _, err := jpeg.Decode(bytes.NewReader(msg[off+4 : off+4+l])); err != nil {
				t.Fatalf("bad jpeg: %v", err)
			}
			off += 4 + l
		} else {
			off += 3
		}
		out = append(out, [5]int{k, x, y, w, h})
	}
	if off != len(msg) {
		t.Fatalf("trailing bytes: %d of %d", off, len(msg))
	}
	return out
}

func TestEncoderSolidAndRefine(t *testing.T) {
	e := &Encoder{Quality: 70, Proto: 2, Adaptive: true}
	f := solid(512, 256, 10, 20, 30)
	rs := parse2(t, mustEncode(t, e, f))
	if len(rs) != 2 || rs[0][0] != 1 || rs[0][3] != 512 { // two 128px bands of solid fill
		t.Fatalf("solid frame: %v", rs)
	}
	// Noise over the whole frame: a motion update at reduced quality …
	noise := solid(512, 256, 0, 0, 0)
	for i := range noise.Pix {
		noise.Pix[i] = byte(i*7 + i/2048)
	}
	rs = parse2(t, mustEncode(t, e, noise))
	for _, r := range rs {
		if r[0] != 0 {
			t.Fatalf("noise should be JPEG: %v", rs)
		}
	}
	// … then nothing for a frame while it settles, then a full-quality refine.
	if msg, _ := e.Encode(noise); msg != nil {
		t.Fatalf("expected no update while settling")
	}
	rs = parse2(t, mustEncode(t, e, noise))
	area := 0
	for _, r := range rs {
		area += r[3] * r[4]
	}
	if area != 512*256 {
		t.Fatalf("refine should cover the frame, got %d px: %v", area, rs)
	}
	if msg, _ := e.Encode(noise); msg != nil {
		t.Fatalf("refined tiles must not be re-sent")
	}
}

func mustEncode(t *testing.T, e *Encoder, f *Frame) []byte {
	t.Helper()
	msg, n := e.Encode(f)
	if n == 0 {
		t.Fatal("expected an update")
	}
	return msg
}

// viewer replays protocol-2 messages onto a BGRA canvas the way the browser does.
type viewer struct{ f *Frame }

func (v *viewer) apply(t *testing.T, msg []byte) (kinds [3]int) {
	t.Helper()
	before := append([]byte(nil), v.f.Pix...) // copies read the screen as it was before the message
	n := int(binary.BigEndian.Uint16(msg[1:]))
	off := 3
	for i := 0; i < n; i++ {
		k := msg[off]
		x, y := int(binary.BigEndian.Uint16(msg[off+1:])), int(binary.BigEndian.Uint16(msg[off+3:]))
		w, h := int(binary.BigEndian.Uint16(msg[off+5:])), int(binary.BigEndian.Uint16(msg[off+7:]))
		off += 9
		kinds[k]++
		for row := 0; row < h; row++ {
			dst := v.f.Pix[(y+row)*v.f.Stride+x*4 : (y+row)*v.f.Stride+(x+w)*4]
			switch k {
			case 0:
				l := int(binary.BigEndian.Uint32(msg[off:]))
				img, err := jpeg.Decode(bytes.NewReader(msg[off+4 : off+4+l]))
				if err != nil {
					t.Fatal(err)
				}
				for col := 0; col < w; col++ {
					r, g, b, _ := img.At(col, row).RGBA()
					dst[col*4], dst[col*4+1], dst[col*4+2], dst[col*4+3] = byte(b>>8), byte(g>>8), byte(r>>8), 255
				}
			case 1:
				for col := 0; col < w; col++ {
					dst[col*4], dst[col*4+1], dst[col*4+2], dst[col*4+3] = msg[off+2], msg[off+1], msg[off], 255
				}
			case 2:
				sx, sy := int(binary.BigEndian.Uint16(msg[off:])), int(binary.BigEndian.Uint16(msg[off+2:]))
				copy(dst, before[(sy+row)*v.f.Stride+sx*4:])
			}
		}
		switch k {
		case 0:
			off += 4 + int(binary.BigEndian.Uint32(msg[off:]))
		case 1:
			off += 3
		case 2:
			off += 4
		}
	}
	return kinds
}

// page draws a scrollable "document": unique rows of text-like marks, offset by scroll.
func page(w, h, scroll int) *Frame {
	f := solid(w, h, 250, 250, 250)
	for y := 0; y < h; y++ {
		line := y + scroll
		for x := 0; x < w; x++ {
			if x < 100 { // a fixed sidebar that doesn't scroll
				i := y*f.Stride + x*4
				f.Pix[i], f.Pix[i+1], f.Pix[i+2] = 40, 30, 20
				continue
			}
			if (line/18)%2 == 0 && (x*7+line*13+(line*line)%97+x*x/31)%11 < 4 {
				i := y*f.Stride + x*4
				f.Pix[i], f.Pix[i+1], f.Pix[i+2] = byte(line), byte(x), 30
			}
		}
	}
	return f
}

func TestEncoderScroll(t *testing.T) {
	e := &Encoder{Quality: 80, Proto: 2, Adaptive: true}
	v := &viewer{f: solid(640, 480, 0, 0, 0)}
	v.apply(t, mustEncode(t, e, page(640, 480, 0)))
	full := len(mustEncodeFresh(t, page(640, 480, 40)))
	before := &Frame{W: 640, H: 480, Stride: 640 * 4, Pix: append([]byte(nil), v.f.Pix...)}
	msg := mustEncode(t, e, page(640, 480, 40)) // scrolled down 40 px
	k := v.apply(t, msg)
	if k[2] == 0 {
		t.Fatalf("no copy rects for a scroll: %v", k)
	}
	if len(msg)*3 > full {
		t.Fatalf("scroll update is %d bytes, a full repaint %d", len(msg), full)
	}
	// The scrolled area is the viewer's previous image moved up 40 rows, pixel for pixel.
	for y := 0; y < 384; y++ { // whole tiles whose source is on screen
		got := v.f.Pix[y*v.f.Stride+400 : y*v.f.Stride+600]
		was := before.Pix[(y+40)*v.f.Stride+400 : (y+40)*v.f.Stride+600]
		if !bytes.Equal(got, was) {
			t.Fatalf("row %d was not moved exactly", y)
		}
	}
	t.Logf("scroll: %d bytes vs %d for a repaint, kinds %v", len(msg), full, k)
}

func mustEncodeFresh(t *testing.T, f *Frame) []byte {
	return mustEncode(t, &Encoder{Quality: 80, Proto: 2, Adaptive: true}, f)
}
