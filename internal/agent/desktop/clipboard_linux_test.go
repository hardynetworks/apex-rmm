//go:build linux

package desktop

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// Needs an X server: DISPLAY=:99 APEX_X11_TEST=1 go test -run Clipboard
func TestX11Clipboard(t *testing.T) {
	if os.Getenv("APEX_X11_TEST") == "" {
		t.Skip("APEX_X11_TEST not set")
	}
	a, err := newClipboard()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := newClipboard()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	seq := b.Seq()
	want := "héllo from Apex ✓\nline 2"
	if err := a.SetText(want); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if b.Seq() == seq {
		t.Fatal("the other client didn't see the clipboard change")
	}
	got, err := b.Text()
	if err != nil || got != want {
		t.Fatalf("read %q, %v", got, err)
	}
	if a.Seq() != 0 {
		t.Fatalf("own change counted: %d", a.Seq())
	}
}

// A command-line clipboard for the E2E tests: APEX_CLIP_SET=text holds the clipboard for
// APEX_CLIP_HOLD seconds; otherwise prints the clipboard text.
func TestClipTool(t *testing.T) {
	if os.Getenv("APEX_CLIP_TOOL") == "" {
		t.Skip()
	}
	c, err := newClipboard()
	if err != nil {
		t.Fatal(err)
	}
	if s := os.Getenv("APEX_CLIP_SET"); s != "" {
		if err := c.SetText(s); err != nil {
			t.Fatal(err)
		}
		d, _ := time.ParseDuration(os.Getenv("APEX_CLIP_HOLD") + "s")
		time.Sleep(d)
		return
	}
	s, err := c.Text()
	fmt.Printf("CLIP<%s>%v\n", s, err)
}
