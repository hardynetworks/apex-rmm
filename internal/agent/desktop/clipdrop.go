package desktop

import "errors"

// clipboard is the remote computer's clipboard (implemented per OS).
type clipboard interface {
	Text() (string, error)
	SetText(s string) error
	SetFiles(paths []string) error // a file list Ctrl+V pastes into programs (Windows only)
	Seq() uint64                   // changes whenever the clipboard content changes
	Close()
}

const maxClipBytes = 1 << 20 // clipboard text larger than this isn't synced

var (
	errNoText      = errors.New("no text on the clipboard")
	errUnsupported = errors.New("not supported on this platform")
)

// dropTarget is where files dropped onto the remote screen are saved.
type dropTarget struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Kind  string `json:"kind"` // "folder" (a file manager window), "desktop", or "app" (saved to Downloads)
}
