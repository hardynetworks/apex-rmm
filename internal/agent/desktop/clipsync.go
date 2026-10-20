package desktop

import (
	"log"
	"sync"
	"time"
)

// clipSync keeps the viewer's and the remote computer's clipboards in step while enabled: text
// copied on the remote side is sent to the viewer, and text the viewer pastes is put on the
// remote clipboard first.
type clipSync struct {
	c       *conn
	mu      sync.Mutex
	cb      clipboard
	cbErr   error
	opened  bool
	on      bool
	last    string // last text sent either way (not echoed back)
	stop    chan struct{}
	stopped bool
}

func newClipSync(c *conn) *clipSync {
	s := &clipSync{c: c, stop: make(chan struct{})}
	go s.watch()
	return s
}

func (s *clipSync) board() clipboard {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.opened {
		s.opened = true
		s.cb, s.cbErr = newClipboard()
		if s.cbErr != nil {
			log.Printf("clipboard unavailable: %v", s.cbErr)
		}
	}
	return s.cb
}

func (s *clipSync) enable(on bool) {
	s.mu.Lock()
	s.on = on
	s.mu.Unlock()
}

func (s *clipSync) enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

func (s *clipSync) fromViewer(text string) {
	if !s.enabled() || len(text) > maxClipBytes {
		return
	}
	cb := s.board()
	if cb == nil {
		return
	}
	s.mu.Lock()
	s.last = text
	s.mu.Unlock()
	if err := cb.SetText(text); err != nil {
		log.Printf("clipboard: %v", err)
	}
}

func (s *clipSync) setFiles(paths []string) error {
	cb := s.board()
	if cb == nil {
		return errUnsupported
	}
	return cb.SetFiles(paths)
}

func (s *clipSync) watch() {
	var seq uint64
	first := true
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		if !s.enabled() {
			first = true
			continue
		}
		cb := s.board()
		if cb == nil {
			return
		}
		n := cb.Seq()
		if first { // don't push whatever was on the clipboard before the session
			seq, first = n, false
			continue
		}
		if n == seq {
			continue
		}
		seq = n
		text, err := cb.Text()
		if err != nil || text == "" || len(text) > maxClipBytes {
			continue
		}
		s.mu.Lock()
		dup := text == s.last
		s.last = text
		s.mu.Unlock()
		if !dup {
			_ = s.c.json(map[string]any{"t": "clip", "text": text})
		}
	}
}

func (s *clipSync) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
	if s.cb != nil {
		s.cb.Close()
	}
}
