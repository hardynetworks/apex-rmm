package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hardynetworks/apex-rmm/internal/proto"
)

// Hub tracks connected agents plus live terminal and remote desktop sessions.
type Hub struct {
	s         *Server
	mu        sync.RWMutex
	agents    map[string]*agentConn
	terminals map[string]*termSession
	desktops  map[string]*desktopSession
}

func newHub(s *Server) *Hub {
	return &Hub{s: s, agents: map[string]*agentConn{}, terminals: map[string]*termSession{}, desktops: map[string]*desktopSession{}}
}

type agentConn struct {
	hub      *Hub
	deviceID string
	ws       *websocket.Conn
	send     chan []byte
	done     chan struct{}
	once     sync.Once
	pmu      sync.Mutex
	pending  map[string]chan proto.Envelope
	lastMet  time.Time
}

// Online reports whether the device has a live control connection.
func (h *Hub) Online(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.agents[deviceID]
	return ok
}

func (h *Hub) get(deviceID string) *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[deviceID]
}

// ErrOffline is returned when an agent is not connected.
var ErrOffline = errors.New("device is offline")

// Send fires a message to an agent without waiting for a reply.
func (h *Hub) Send(deviceID, typ string, v any) error {
	a := h.get(deviceID)
	if a == nil {
		return ErrOffline
	}
	b, err := proto.Marshal(typ, "", v)
	if err != nil {
		return err
	}
	return a.enqueue(b)
}

// Request sends a message and waits for the agent's result.
func (h *Hub) Request(ctx context.Context, deviceID, typ string, v any, timeout time.Duration) (json.RawMessage, error) {
	a := h.get(deviceID)
	if a == nil {
		return nil, ErrOffline
	}
	id := randToken(9)
	ch := make(chan proto.Envelope, 1)
	a.pmu.Lock()
	a.pending[id] = ch
	a.pmu.Unlock()
	defer func() {
		a.pmu.Lock()
		delete(a.pending, id)
		a.pmu.Unlock()
	}()
	b, err := proto.Marshal(typ, id, v)
	if err != nil {
		return nil, err
	}
	if err := a.enqueue(b); err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case env := <-ch:
		if env.Error != "" {
			return nil, &httpError{502, env.Error}
		}
		return env.Data, nil
	case <-t.C:
		return nil, &httpError{504, "agent did not respond in time"}
	case <-a.done:
		return nil, ErrOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *agentConn) enqueue(b []byte) error {
	select {
	case a.send <- b:
		return nil
	case <-a.done:
		return ErrOffline
	case <-time.After(10 * time.Second):
		return errors.New("agent send queue full")
	}
}

func (a *agentConn) close() {
	a.once.Do(func() {
		close(a.done)
		_ = a.ws.Close()
	})
}

// serveAgent runs the control connection for an authenticated agent.
func (h *Hub) serveAgent(deviceID, ip string, ws *websocket.Conn) {
	a := &agentConn{hub: h, deviceID: deviceID, ws: ws, send: make(chan []byte, 256), done: make(chan struct{}), pending: map[string]chan proto.Envelope{}}
	h.mu.Lock()
	if old := h.agents[deviceID]; old != nil {
		old.close()
	}
	h.agents[deviceID] = a
	h.mu.Unlock()

	ctx := context.Background()
	_, _ = h.s.db.Exec(ctx, `UPDATE devices SET online=true, last_seen=now(), public_ip=$2 WHERE id=$1`, deviceID, ip)
	slog.Info("agent connected", "device", deviceID, "ip", ip)

	go a.writeLoop()
	go h.s.dispatchPending(deviceID)
	a.readLoop()

	a.close()
	h.mu.Lock()
	if h.agents[deviceID] == a {
		delete(h.agents, deviceID)
		h.mu.Unlock()
		_, _ = h.s.db.Exec(ctx, `UPDATE devices SET online=false, last_seen=now() WHERE id=$1`, deviceID)
		slog.Info("agent disconnected", "device", deviceID)
	} else {
		h.mu.Unlock()
	}
	// terminate terminals bound to this agent
	h.mu.Lock()
	for id, t := range h.terminals {
		if t.deviceID == deviceID {
			t.closeViewer("agent disconnected")
			delete(h.terminals, id)
		}
	}
	h.mu.Unlock()
}

func (a *agentConn) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b := <-a.send:
			_ = a.ws.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if err := a.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				a.close()
				return
			}
		case <-ping.C:
			_ = a.ws.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if err := a.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				a.close()
				return
			}
		case <-a.done:
			return
		}
	}
}

func (a *agentConn) readLoop() {
	a.ws.SetReadLimit(16 << 20)
	_ = a.ws.SetReadDeadline(time.Now().Add(120 * time.Second))
	a.ws.SetPongHandler(func(string) error {
		return a.ws.SetReadDeadline(time.Now().Add(120 * time.Second))
	})
	for {
		_, msg, err := a.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = a.ws.SetReadDeadline(time.Now().Add(120 * time.Second))
		var env proto.Envelope
		if json.Unmarshal(msg, &env) != nil {
			continue
		}
		a.handle(env)
	}
}

func (a *agentConn) handle(env proto.Envelope) {
	s := a.hub.s
	ctx := context.Background()
	switch env.Type {
	case proto.TypeResult:
		a.pmu.Lock()
		ch := a.pending[env.ID]
		a.pmu.Unlock()
		if ch != nil {
			select {
			case ch <- env:
			default:
			}
		}
	case proto.TypeHello:
		var h proto.Hello
		if json.Unmarshal(env.Data, &h) == nil {
			s.updateInventory(ctx, a.deviceID, &h.Inventory)
		}
	case proto.TypeMetrics:
		var m proto.Metrics
		if json.Unmarshal(env.Data, &m) == nil {
			s.recordMetrics(ctx, a.deviceID, &m)
		}
	case proto.TypeScriptResult:
		var r proto.ScriptResult
		if json.Unmarshal(env.Data, &r) == nil {
			s.recordScriptResult(ctx, a.deviceID, &r)
		}
	case proto.TypeTerminalOutput, proto.TypeTerminalExit:
		var td proto.TerminalData
		if json.Unmarshal(env.Data, &td) != nil {
			return
		}
		a.hub.mu.RLock()
		t := a.hub.terminals[td.SessionID]
		a.hub.mu.RUnlock()
		if t == nil || t.deviceID != a.deviceID {
			return
		}
		if env.Type == proto.TypeTerminalExit {
			t.closeViewer("shell exited")
			return
		}
		t.write(websocket.BinaryMessage, td.Data)
	case proto.TypeLog:
		slog.Info("agent log", "device", a.deviceID, "msg", string(env.Data))
	}
}

// ---- terminal sessions ----

type termSession struct {
	id       string
	deviceID string
	viewer   *websocket.Conn
	wmu      sync.Mutex
}

func (t *termSession) write(mt int, b []byte) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_ = t.viewer.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = t.viewer.WriteMessage(mt, b)
}

func (t *termSession) closeViewer(reason string) {
	t.write(websocket.TextMessage, []byte(`{"t":"exit","reason":`+jsonString(reason)+`}`))
	_ = t.viewer.Close()
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runTerminal bridges a browser terminal to an agent shell.
func (h *Hub) runTerminal(deviceID string, viewer *websocket.Conn, shell string, cols, rows int) {
	t := &termSession{id: randToken(12), deviceID: deviceID, viewer: viewer}
	h.mu.Lock()
	h.terminals[t.id] = t
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.terminals, t.id)
		h.mu.Unlock()
		_ = h.Send(deviceID, proto.TypeTerminalClose, proto.TerminalData{SessionID: t.id})
		_ = viewer.Close()
	}()
	if _, err := h.Request(context.Background(), deviceID, proto.TypeTerminalOpen, proto.TerminalOpen{SessionID: t.id, Shell: shell, Cols: cols, Rows: rows}, 20*time.Second); err != nil {
		t.write(websocket.TextMessage, []byte(`{"t":"error","error":`+jsonString(err.Error())+`}`))
		return
	}
	t.write(websocket.TextMessage, []byte(`{"t":"ready"}`))
	viewer.SetReadLimit(1 << 20)
	for {
		_, msg, err := viewer.ReadMessage()
		if err != nil {
			return
		}
		var in struct {
			T    string `json:"t"`
			D    string `json:"d"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(msg, &in) != nil {
			continue
		}
		switch in.T {
		case "i":
			if err := h.Send(deviceID, proto.TypeTerminalInput, proto.TerminalData{SessionID: t.id, Data: []byte(in.D)}); err != nil {
				return
			}
		case "r":
			_ = h.Send(deviceID, proto.TypeTerminalResize, proto.TerminalData{SessionID: t.id, Cols: in.Cols, Rows: in.Rows})
		}
	}
}

// ---- remote desktop sessions ----

type desktopSession struct {
	id        string
	token     string
	deviceID  string
	userID    string
	created   time.Time
	agentConn *websocket.Conn
	ready     chan struct{}
	done      chan struct{}
	once      sync.Once
	readyOnce sync.Once
}

func (d *desktopSession) finish() {
	d.once.Do(func() { close(d.done) })
}

// newDesktop creates a session and asks the agent to spawn its desktop helper.
func (h *Hub) newDesktop(ctx context.Context, deviceID, userID string) (*desktopSession, error) {
	d := &desktopSession{id: randToken(16), token: randToken(32), deviceID: deviceID, userID: userID, created: time.Now(), ready: make(chan struct{}), done: make(chan struct{})}
	h.mu.Lock()
	h.desktops[d.id] = d
	h.mu.Unlock()
	_, err := h.Request(ctx, deviceID, proto.TypeDesktopStart, proto.DesktopStart{SessionID: d.id, Token: d.token, URL: h.s.conf().WSURL() + "/api/agent/desktop"}, 30*time.Second)
	if err != nil {
		h.dropDesktop(d)
		return nil, err
	}
	// Garbage-collect sessions that are never joined.
	go func() {
		select {
		case <-d.ready:
		case <-d.done:
		case <-time.After(90 * time.Second):
			h.dropDesktop(d)
		}
	}()
	return d, nil
}

func (h *Hub) dropDesktop(d *desktopSession) {
	h.mu.Lock()
	delete(h.desktops, d.id)
	h.mu.Unlock()
	d.finish()
}

func (h *Hub) desktop(id string) *desktopSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.desktops[id]
}

// attachDesktopAgent is called when the helper process connects.
func (h *Hub) attachDesktopAgent(d *desktopSession, ws *websocket.Conn) {
	d.agentConn = ws
	d.readyOnce.Do(func() { close(d.ready) })
	<-d.done
	_ = ws.Close()
}

// runDesktopViewer pipes frames between the browser and the helper.
func (h *Hub) runDesktopViewer(d *desktopSession, viewer *websocket.Conn) {
	defer func() {
		h.dropDesktop(d)
		_ = viewer.Close()
	}()
	select {
	case <-d.ready:
	case <-d.done:
		return
	case <-time.After(45 * time.Second):
		_ = viewer.WriteMessage(websocket.TextMessage, []byte(`{"t":"error","error":"The device did not start a desktop session. Check that a user session exists (Linux needs Xorg; macOS needs Screen Recording permission)."}`))
		return
	}
	agent := d.agentConn
	agent.SetReadLimit(64 << 20)
	viewer.SetReadLimit(1 << 20)
	go func() {
		defer d.finish()
		for {
			mt, b, err := agent.ReadMessage()
			if err != nil {
				_ = viewer.WriteMessage(websocket.TextMessage, []byte(`{"t":"error","error":"desktop session ended"}`))
				return
			}
			_ = viewer.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := viewer.WriteMessage(mt, b); err != nil {
				return
			}
		}
	}()
	go func() {
		<-d.done
		_ = viewer.Close()
		_ = agent.Close()
	}()
	for {
		mt, b, err := viewer.ReadMessage()
		if err != nil {
			d.finish()
			return
		}
		_ = agent.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := agent.WriteMessage(mt, b); err != nil {
			d.finish()
			return
		}
	}
}

// OnlineCount returns the number of connected agents.
func (h *Hub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.agents)
}

func shellFor(os, requested string) string {
	if requested != "" {
		return requested
	}
	switch strings.ToLower(os) {
	case "windows":
		return "powershell"
	case "darwin":
		return "zsh"
	}
	return "bash"
}

var _ = fmt.Sprintf
