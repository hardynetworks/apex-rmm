// Package mattertest is a small fake of the Matter controller's WebSocket API for tests.
package mattertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Server fakes matterjs-server: start_listening, commission_with_code, device_command,
// remove_node, plus events pushed with SetAttr.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	nodes    map[int64]map[string]any        // node id -> attributes
	conns    map[*websocket.Conn]*sync.Mutex // one writer per connection
	codes    map[string]map[string]any       // pairing code -> attributes of the device it adds
	next     int64
	Commands []string // "node/endpoint/cluster/command" received
}

// New starts a fake controller. URL() is its ws:// address.
func New() *Server {
	f := &Server{nodes: map[int64]map[string]any{}, conns: map[*websocket.Conn]*sync.Mutex{}, codes: map[string]map[string]any{}, next: 1}
	up := websocket.Upgrader{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns[c] = &sync.Mutex{}
		f.mu.Unlock()
		f.write(c, map[string]any{"fabric_id": 1, "schema_version": 11, "sdk_version": "fake"})
		f.serve(c)
	}))
	return f
}

// URL is the WebSocket URL of the fake.
func (f *Server) URL() string { return "ws" + strings.TrimPrefix(f.Server.URL, "http") + "/ws" }

// AddPairingCode makes a code commission a device with these attributes.
func (f *Server) AddPairingCode(code string, attrs map[string]any) {
	f.mu.Lock()
	f.codes[code] = attrs
	f.mu.Unlock()
}

// AddNode adds an already-commissioned node.
func (f *Server) AddNode(attrs map[string]any) int64 {
	f.mu.Lock()
	id := f.next
	f.next++
	f.nodes[id] = attrs
	f.mu.Unlock()
	return id
}

// SetAttr changes an attribute and sends attribute_updated to listeners.
func (f *Server) SetAttr(id int64, path string, v any) {
	f.mu.Lock()
	if f.nodes[id] != nil {
		f.nodes[id][path] = v
	}
	f.mu.Unlock()
	f.broadcast(map[string]any{"event": "attribute_updated", "data": []any{id, path, v}})
}

// Attr reads an attribute.
func (f *Server) Attr(id int64, path string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[id][path]
}

func (f *Server) broadcast(m any) {
	f.mu.Lock()
	var cs []*websocket.Conn
	for c := range f.conns {
		cs = append(cs, c)
	}
	f.mu.Unlock()
	for _, c := range cs {
		f.write(c, m)
	}
}

func (f *Server) write(c *websocket.Conn, m any) {
	f.mu.Lock()
	l := f.conns[c]
	f.mu.Unlock()
	if l == nil {
		return
	}
	l.Lock()
	_ = c.WriteJSON(m)
	l.Unlock()
}

func (f *Server) node(id int64) map[string]any {
	return map[string]any{"node_id": id, "available": true, "attributes": f.nodes[id]}
}

func (f *Server) serve(c *websocket.Conn) {
	defer func() {
		f.mu.Lock()
		delete(f.conns, c)
		f.mu.Unlock()
		c.Close()
	}()
	for {
		var m struct {
			MessageID string          `json:"message_id"`
			Command   string          `json:"command"`
			Args      json.RawMessage `json:"args"`
		}
		if err := c.ReadJSON(&m); err != nil {
			return
		}
		reply := func(result any) { f.write(c, map[string]any{"message_id": m.MessageID, "result": result}) }
		fail := func(code int, msg string) {
			f.write(c, map[string]any{"message_id": m.MessageID, "error_code": code, "details": msg})
		}
		switch m.Command {
		case "start_listening":
			f.mu.Lock()
			var out []any
			for id := range f.nodes {
				out = append(out, f.node(id))
			}
			f.mu.Unlock()
			if out == nil {
				out = []any{}
			}
			reply(out)
		case "commission_with_code":
			var a struct {
				Code        string `json:"code"`
				NetworkOnly bool   `json:"network_only"`
			}
			_ = json.Unmarshal(m.Args, &a)
			f.mu.Lock()
			attrs := f.codes[a.Code]
			f.mu.Unlock()
			if attrs == nil {
				fail(3, "commissioning failed: device not found")
				continue
			}
			id := f.AddNode(attrs)
			f.mu.Lock()
			n := f.node(id)
			f.mu.Unlock()
			reply(n)
			f.broadcast(map[string]any{"event": "node_added", "data": n})
		case "device_command":
			var a struct {
				NodeID     int64  `json:"node_id"`
				EndpointID int    `json:"endpoint_id"`
				ClusterID  int    `json:"cluster_id"`
				Command    string `json:"command_name"`
			}
			_ = json.Unmarshal(m.Args, &a)
			f.mu.Lock()
			f.Commands = append(f.Commands, strings.Join([]string{itoa(a.NodeID), itoa(int64(a.EndpointID)), itoa(int64(a.ClusterID)), a.Command}, "/"))
			ok := f.nodes[a.NodeID] != nil
			f.mu.Unlock()
			if !ok {
				fail(5, "node not found")
				continue
			}
			reply(nil)
			if a.ClusterID == 6 {
				f.SetAttr(a.NodeID, itoa(int64(a.EndpointID))+"/6/0", a.Command == "On")
				if a.Command == "Off" {
					f.SetAttr(a.NodeID, itoa(int64(a.EndpointID))+"/144/8", 0)
				}
			}
		case "remove_node":
			var a struct {
				NodeID int64 `json:"node_id"`
			}
			_ = json.Unmarshal(m.Args, &a)
			f.mu.Lock()
			delete(f.nodes, a.NodeID)
			f.mu.Unlock()
			reply(nil)
			f.broadcast(map[string]any{"event": "node_removed", "data": a.NodeID})
		default:
			fail(1, "unknown command "+m.Command)
		}
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
