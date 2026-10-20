package desktop

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// File transfer during a remote-control session. It shares the session's websocket:
//
//	viewer → agent (JSON)  {"t":"fls","id":1,"path":"…"}                  list a folder ("" = home)
//	                       {"t":"fget","id":2,"path":"…"}                 download a file
//	                       {"t":"fput","id":3,"path":"<dir>","name":"…","size":123}  start an upload
//	                       {"t":"fend","id":3}                            upload complete
//	                       {"t":"fack","id":2,"n":<bytes received>}       download flow control
//	                       {"t":"fcancel","id":2}
//	viewer → agent (binary) 0x11 | u32 id | data                         upload chunk
//	agent → viewer (JSON)  {"t":"fls"|"fget"|"fput"|"fdone"|"ferr", "id":…, …}
//	                       {"t":"fack","id":3,"n":<bytes written>}        upload flow control
//	agent → viewer (binary) 0x10 | u32 id | data                         download chunk
const (
	fileChunk      = 256 << 10
	fileWindow     = 8 * fileChunk // bytes in flight before waiting for an ack
	maxListEntries = 5000
	tagDownload    = 0x10
	tagUpload      = 0x11
)

func isFileMsg(t string) bool {
	switch t {
	case "fls", "fget", "fput", "fend", "fack", "fcancel", "fmkdir":
		return true
	}
	return false
}

type fileMsg struct {
	T    string `json:"t"`
	ID   uint32 `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	N    int64  `json:"n"`
	Sub  string `json:"sub"` // upload into this sub-folder of Path (folders dragged from the viewer's computer)
}

type fileEntry struct {
	Name  string `json:"n"`
	Dir   bool   `json:"d,omitempty"`
	Size  int64  `json:"s"`
	MTime int64  `json:"m"`
}

type download struct {
	acked  int64
	cond   *sync.Cond
	cancel bool
}

type upload struct {
	f       *os.File
	path    string
	size    int64
	written int64
}

type fileManager struct {
	c         *conn
	mu        sync.Mutex
	downloads map[uint32]*download
	uploads   map[uint32]*upload
	work      chan func() // uploads are written in order on one goroutine
	closed    bool
}

func newFileManager(c *conn) *fileManager {
	m := &fileManager{c: c, downloads: map[uint32]*download{}, uploads: map[uint32]*upload{}, work: make(chan func(), 64)}
	go func() {
		for fn := range m.work {
			fn()
		}
	}()
	return m
}

// do queues fn on the upload goroutine (dropped once the session is closing).
func (m *fileManager) do(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.work <- fn
	}
}

func (m *fileManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for _, d := range m.downloads {
		d.cancel = true
		d.cond.Broadcast()
	}
	m.work <- func() {
		for id, u := range m.uploads {
			u.f.Close()
			os.Remove(u.path) // incomplete
			delete(m.uploads, id)
		}
	}
	close(m.work)
}

func (m *fileManager) fail(id uint32, err error) {
	_ = m.c.json(map[string]any{"t": "ferr", "id": id, "error": friendlyErr(err)})
}

// control handles a JSON file message from the viewer (called on the websocket reader goroutine,
// so anything slow happens elsewhere).
func (m *fileManager) control(b []byte) {
	var r fileMsg
	if json.Unmarshal(b, &r) != nil {
		return
	}
	switch r.T {
	case "fls":
		go m.list(r)
	case "fget":
		go m.download(r)
	case "fack":
		m.mu.Lock()
		if d := m.downloads[r.ID]; d != nil {
			d.acked = r.N
			d.cond.Broadcast()
		}
		m.mu.Unlock()
	case "fcancel":
		m.mu.Lock()
		if d := m.downloads[r.ID]; d != nil {
			d.cancel = true
			d.cond.Broadcast()
		}
		m.mu.Unlock()
		m.do(func() {
			if u := m.uploads[r.ID]; u != nil {
				u.f.Close()
				os.Remove(u.path)
				delete(m.uploads, r.ID)
			}
		})
	case "fput":
		m.do(func() { m.startUpload(r) })
	case "fmkdir":
		m.do(func() { m.mkdir(r) })
	case "fend":
		m.do(func() { m.finishUpload(r.ID) })
	}
}

// binary handles an upload chunk.
func (m *fileManager) binary(b []byte) {
	if len(b) < 5 || b[0] != tagUpload {
		return
	}
	id := binary.BigEndian.Uint32(b[1:])
	data := append([]byte(nil), b[5:]...)
	m.do(func() {
		u := m.uploads[id]
		if u == nil {
			return
		}
		if u.written+int64(len(data)) > u.size {
			m.abortUpload(id, errors.New("received more data than announced"))
			return
		}
		if _, err := u.f.Write(data); err != nil {
			m.abortUpload(id, err)
			return
		}
		u.written += int64(len(data))
		_ = m.c.json(map[string]any{"t": "fack", "id": id, "n": u.written})
	})
}

func (m *fileManager) abortUpload(id uint32, err error) {
	if u := m.uploads[id]; u != nil {
		u.f.Close()
		os.Remove(u.path)
		delete(m.uploads, id)
	}
	m.fail(id, err)
}

func (m *fileManager) list(r fileMsg) {
	dir := r.Path
	if dir == "" {
		dir = userHome()
	}
	dir = filepath.Clean(dir)
	des, err := os.ReadDir(dir)
	if err != nil {
		m.fail(r.ID, err)
		return
	}
	entries := make([]fileEntry, 0, len(des))
	for _, de := range des {
		if len(entries) >= maxListEntries {
			break
		}
		e := fileEntry{Name: de.Name(), Dir: de.IsDir()}
		if info, err := de.Info(); err == nil {
			e.MTime = info.ModTime().Unix()
			if !e.Dir {
				e.Size = info.Size()
			}
			if de.Type()&os.ModeSymlink != 0 { // show links to folders as folders
				if st, err := os.Stat(filepath.Join(dir, de.Name())); err == nil && st.IsDir() {
					e.Dir = true
				}
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := filepath.Dir(dir)
	if parent == dir {
		parent = ""
	}
	_ = m.c.json(map[string]any{
		"t": "fls", "id": r.ID, "path": dir, "parent": parent, "entries": entries,
		"home": userHome(), "roots": roots(), "sep": string(filepath.Separator), "truncated": len(des) > len(entries),
	})
}

func (m *fileManager) download(r fileMsg) {
	f, err := os.Open(filepath.Clean(r.Path))
	if err != nil {
		m.fail(r.ID, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		m.fail(r.ID, err)
		return
	}
	if st.IsDir() {
		m.fail(r.ID, errors.New("folders can't be downloaded; download the files inside it instead"))
		return
	}
	d := &download{}
	d.cond = sync.NewCond(&m.mu)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.downloads[r.ID] = d
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.downloads, r.ID)
		m.mu.Unlock()
	}()
	if err := m.c.json(map[string]any{"t": "fget", "id": r.ID, "name": st.Name(), "size": st.Size()}); err != nil {
		return
	}
	buf := make([]byte, 5+fileChunk)
	buf[0] = tagDownload
	binary.BigEndian.PutUint32(buf[1:], r.ID)
	var sent int64
	for {
		m.mu.Lock()
		for !d.cancel && sent-d.acked >= fileWindow {
			d.cond.Wait()
		}
		cancel := d.cancel
		m.mu.Unlock()
		if cancel {
			return
		}
		n, err := f.Read(buf[5:])
		if n > 0 {
			if werr := m.c.write(websocket.BinaryMessage, buf[:5+n]); werr != nil {
				return
			}
			sent += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			m.fail(r.ID, err)
			return
		}
	}
	_ = m.c.json(map[string]any{"t": "fdone", "id": r.ID, "size": sent})
}

func (m *fileManager) startUpload(r fileMsg) {
	name := filepath.Base(strings.ReplaceAll(r.Name, "\\", "/"))
	if name == "." || name == "/" || name == ".." || strings.TrimSpace(name) == "" {
		m.fail(r.ID, errors.New("invalid file name"))
		return
	}
	dir := r.Path
	if dir == "" {
		dir = userHome()
	}
	dir = filepath.Clean(dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		m.fail(r.ID, errors.New("the destination folder doesn't exist"))
		return
	}
	if r.Sub != "" {
		sub, err := safeRelative(r.Sub)
		if err != nil {
			m.fail(r.ID, err)
			return
		}
		if err := mkdirAllForUser(dir, sub); err != nil {
			m.fail(r.ID, err)
			return
		}
		dir = filepath.Join(dir, sub)
	}
	path, f, err := createUnique(dir, name)
	if err != nil {
		m.fail(r.ID, err)
		return
	}
	m.uploads[r.ID] = &upload{f: f, path: path, size: r.Size}
	_ = m.c.json(map[string]any{"t": "fput", "id": r.ID, "path": path, "name": filepath.Base(path)})
}

func (m *fileManager) finishUpload(id uint32) {
	u := m.uploads[id]
	if u == nil {
		return
	}
	delete(m.uploads, id)
	if u.written != u.size {
		u.f.Close()
		os.Remove(u.path)
		m.fail(id, errors.New("upload was incomplete"))
		return
	}
	if err := u.f.Close(); err != nil {
		os.Remove(u.path)
		m.fail(id, err)
		return
	}
	giveToUser(u.path)
	_ = m.c.json(map[string]any{"t": "fdone", "id": id, "path": u.path, "size": u.written})
}

// createUnique creates dir/name, or "name (1).ext", "name (2).ext" … if it already exists,
// so an upload never overwrites a file on the device.
func createUnique(dir, name string) (string, *os.File, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		n := name
		if i > 0 {
			n = stem + " (" + itoa(i) + ")" + ext
		}
		p := filepath.Join(dir, n)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return p, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("too many files with that name")
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func friendlyErr(err error) string {
	switch {
	case os.IsNotExist(err):
		return "not found"
	case os.IsPermission(err):
		return "access denied"
	}
	s := err.Error()
	if pe, ok := err.(*os.PathError); ok {
		s = pe.Err.Error()
	}
	return s
}

var (
	homeOnce sync.Once
	homeDir  string
)

// userHome is the signed-in user's home folder (the helper itself runs as SYSTEM/root).
func userHome() string {
	homeOnce.Do(func() {
		homeDir = consoleUserHome()
		if homeDir == "" {
			homeDir, _ = os.UserHomeDir()
		}
		if homeDir == "" {
			homeDir = string(filepath.Separator)
		}
	})
	return homeDir
}

// mkdir creates a new folder (made unique like files: "Photos (1)") for a dragged-in folder.
func (m *fileManager) mkdir(r fileMsg) {
	name := filepath.Base(strings.ReplaceAll(r.Name, "\\", "/"))
	if name == "." || name == "/" || name == ".." || strings.TrimSpace(name) == "" {
		m.fail(r.ID, errors.New("invalid folder name"))
		return
	}
	dir := filepath.Clean(r.Path)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		m.fail(r.ID, errors.New("the destination folder doesn't exist"))
		return
	}
	for i := 0; i < 1000; i++ {
		n := name
		if i > 0 {
			n = name + " (" + itoa(i) + ")"
		}
		p := filepath.Join(dir, n)
		err := os.Mkdir(p, 0o755)
		if err == nil {
			giveToUser(p)
			_ = m.c.json(map[string]any{"t": "fmkdir", "id": r.ID, "path": p, "name": n})
			return
		}
		if !os.IsExist(err) {
			m.fail(r.ID, err)
			return
		}
	}
	m.fail(r.ID, errors.New("too many folders with that name"))
}

// safeRelative cleans a relative path from the viewer, refusing anything that would leave the
// destination folder.
func safeRelative(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	var parts []string
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", errors.New("invalid folder path")
		}
		if strings.ContainsAny(s, ":") || strings.TrimSpace(s) == "" {
			return "", errors.New("invalid folder path")
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "", errors.New("invalid folder path")
	}
	return filepath.Join(parts...), nil
}

// mkdirAllForUser creates base/sub's missing folders, owned by the signed-in user.
func mkdirAllForUser(base, sub string) error {
	cur := base
	for _, part := range strings.Split(sub, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, 0o755); err == nil {
			giveToUser(cur)
		} else if !os.IsExist(err) {
			return err
		}
	}
	return nil
}
