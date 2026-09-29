package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hardynetworks/hardy-rmm/internal/proto"
	"github.com/shirou/gopsutil/v4/process"
)

type ptyProc interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// Agent is the long-running endpoint process.
type Agent struct {
	cfg  *Config
	ws   *websocket.Conn
	wmu  sync.Mutex
	tmu  sync.Mutex
	ptys map[string]ptyProc
	mc   *metricsCollector
}

// New creates an agent from config.
func New(cfg *Config) *Agent {
	return &Agent{cfg: cfg, ptys: map[string]ptyProc{}}
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
}

func wsURL(server string) string {
	if strings.HasPrefix(server, "https://") {
		return "wss://" + strings.TrimPrefix(server, "https://")
	}
	return "ws://" + strings.TrimPrefix(server, "http://")
}

// Run connects to the server and reconnects forever until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	a.mc = newMetricsCollector()
	backoff := 2 * time.Second
	go a.updateLoop(ctx)
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		wait := backoff + time.Duration(rand.Int63n(int64(backoff/2+1)))
		log.Printf("connection lost (%v); retrying in %s", err, wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (a *Agent) session(ctx context.Context) error {
	d := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 20 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	h := http.Header{"Authorization": {"Bearer " + a.cfg.DeviceID + "." + a.cfg.Secret}, "User-Agent": {"hardy-agent/" + proto.Version}}
	ws, resp, err := d.DialContext(ctx, wsURL(a.cfg.Server)+"/api/agent/ws", h)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errors.New("server rejected credentials (device deleted?)")
		}
		return err
	}
	defer ws.Close()
	a.wmu.Lock()
	a.ws = ws
	a.wmu.Unlock()
	log.Printf("connected to %s", a.cfg.Server)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sctx.Done()
		_ = ws.Close()
	}()

	a.send(proto.TypeHello, "", proto.Hello{Inventory: CollectInventory()})
	go a.metricsLoop(sctx)

	ws.SetReadLimit(32 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(120 * time.Second))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(120 * time.Second))
		a.wmu.Lock()
		defer a.wmu.Unlock()
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			a.closeAllPTY()
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(120 * time.Second))
		var env proto.Envelope
		if json.Unmarshal(msg, &env) != nil {
			continue
		}
		switch env.Type {
		case proto.TypeTerminalInput, proto.TypeTerminalResize:
			a.handle(env) // keep keystroke order
		default:
			go a.handle(env)
		}
	}
}

func (a *Agent) send(typ, id string, v any) {
	b, err := proto.Marshal(typ, id, v)
	if err != nil {
		return
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.ws == nil {
		return
	}
	_ = a.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := a.ws.WriteMessage(websocket.TextMessage, b); err != nil {
		_ = a.ws.Close()
	}
}

func (a *Agent) reply(id string, v any, err error) {
	if id == "" {
		return
	}
	env := proto.Envelope{Type: proto.TypeResult, ID: id}
	if err != nil {
		env.Error = err.Error()
	} else if v != nil {
		env.Data, _ = json.Marshal(v)
	}
	b, _ := json.Marshal(env)
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.ws != nil {
		_ = a.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = a.ws.WriteMessage(websocket.TextMessage, b)
	}
}

func (a *Agent) metricsLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	inv := time.NewTicker(time.Hour)
	defer inv.Stop()
	time.Sleep(time.Second)
	a.send(proto.TypeMetrics, "", a.mc.collect())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.send(proto.TypeMetrics, "", a.mc.collect())
		case <-inv.C:
			a.send(proto.TypeHello, "", proto.Hello{Inventory: CollectInventory()})
		}
	}
}

func (a *Agent) handle(env proto.Envelope) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic handling %s: %v", env.Type, r)
			a.reply(env.ID, nil, errors.New("agent panic"))
		}
	}()
	switch env.Type {
	case proto.TypePing:
		a.reply(env.ID, map[string]string{"version": proto.Version}, nil)
	case proto.TypeRunScript:
		var rs proto.RunScript
		if json.Unmarshal(env.Data, &rs) == nil {
			log.Printf("running %s script (result %s)", rs.Shell, rs.ResultID)
			a.send(proto.TypeScriptResult, "", runScript(rs))
		}
	case proto.TypeTerminalOpen:
		var t proto.TerminalOpen
		_ = json.Unmarshal(env.Data, &t)
		a.reply(env.ID, nil, a.openPTY(t))
	case proto.TypeTerminalInput:
		var t proto.TerminalData
		if json.Unmarshal(env.Data, &t) == nil {
			if p := a.getPTY(t.SessionID); p != nil {
				_, _ = p.Write(t.Data)
			}
		}
	case proto.TypeTerminalResize:
		var t proto.TerminalData
		if json.Unmarshal(env.Data, &t) == nil && t.Cols > 0 && t.Rows > 0 {
			if p := a.getPTY(t.SessionID); p != nil {
				_ = p.Resize(t.Cols, t.Rows)
			}
		}
	case proto.TypeTerminalClose:
		var t proto.TerminalData
		if json.Unmarshal(env.Data, &t) == nil {
			a.closePTY(t.SessionID)
		}
	case proto.TypeDesktopStart:
		var d proto.DesktopStart
		_ = json.Unmarshal(env.Data, &d)
		a.reply(env.ID, nil, spawnDesktopHelper(d))
	case proto.TypeListProcesses:
		a.reply(env.ID, listProcesses(), nil)
	case proto.TypeKillProcess:
		var k proto.KillProcess
		_ = json.Unmarshal(env.Data, &k)
		p, err := process.NewProcess(k.PID)
		if err == nil {
			err = p.Kill()
		}
		a.reply(env.ID, nil, err)
	case proto.TypeListServices:
		s, err := listServices()
		a.reply(env.ID, s, err)
	case proto.TypeServiceAction:
		var s proto.ServiceAction
		_ = json.Unmarshal(env.Data, &s)
		a.reply(env.ID, nil, serviceAction(s.Name, s.Action))
	case proto.TypeListSoftware:
		s, err := listSoftware()
		a.reply(env.ID, s, err)
	case proto.TypePower:
		var p proto.Power
		_ = json.Unmarshal(env.Data, &p)
		a.reply(env.ID, nil, nil)
		time.Sleep(time.Second)
		if err := powerAction(p.Action, p.Delay); err != nil {
			log.Printf("power action failed: %v", err)
		}
	case proto.TypeRefreshInventory:
		inv := CollectInventory()
		a.send(proto.TypeHello, "", proto.Hello{Inventory: inv})
		a.send(proto.TypeMetrics, "", a.mc.collect())
		time.Sleep(500 * time.Millisecond)
		a.reply(env.ID, nil, nil)
	case proto.TypeUpdateAgent:
		var u proto.UpdateAgent
		_ = json.Unmarshal(env.Data, &u)
		err := selfUpdate(u.URL, u.SHA256)
		a.reply(env.ID, nil, err)
		if err == nil {
			restartSelf()
		}
	case proto.TypeUninstall:
		a.reply(env.ID, nil, nil)
		selfUninstall()
	case proto.TypeRustDesk:
		var p proto.RustDeskProvision
		_ = json.Unmarshal(env.Data, &p)
		res, err := provisionRustDesk(p)
		a.reply(env.ID, res, err)
	default:
		a.reply(env.ID, nil, errors.New("unsupported command: "+env.Type))
	}
}

// ---- terminals ----

func (a *Agent) openPTY(t proto.TerminalOpen) error {
	if t.Cols <= 0 {
		t.Cols = 120
	}
	if t.Rows <= 0 {
		t.Rows = 32
	}
	p, err := startPTY(t.Shell, t.Cols, t.Rows)
	if err != nil {
		return err
	}
	a.tmu.Lock()
	a.ptys[t.SessionID] = p
	a.tmu.Unlock()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				a.send(proto.TypeTerminalOutput, "", proto.TerminalData{SessionID: t.SessionID, Data: append([]byte(nil), buf[:n]...)})
			}
			if err != nil {
				break
			}
		}
		a.closePTY(t.SessionID)
		a.send(proto.TypeTerminalExit, "", proto.TerminalData{SessionID: t.SessionID})
	}()
	return nil
}

func (a *Agent) getPTY(id string) ptyProc {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	return a.ptys[id]
}

func (a *Agent) closePTY(id string) {
	a.tmu.Lock()
	p := a.ptys[id]
	delete(a.ptys, id)
	a.tmu.Unlock()
	if p != nil {
		_ = p.Close()
	}
}

func (a *Agent) closeAllPTY() {
	a.tmu.Lock()
	ids := make([]string, 0, len(a.ptys))
	for id := range a.ptys {
		ids = append(ids, id)
	}
	a.tmu.Unlock()
	for _, id := range ids {
		a.closePTY(id)
	}
}

// ---- processes ----

func listProcesses() []proto.Process {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	out := make([]proto.Process, 0, len(procs))
	for _, p := range procs {
		name, _ := p.Name()
		user, _ := p.Username()
		cpu, _ := p.CPUPercent()
		var rss uint64
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			rss = mi.RSS
		}
		cmd, _ := p.Cmdline()
		if len(cmd) > 400 {
			cmd = cmd[:400]
		}
		out = append(out, proto.Process{PID: p.Pid, Name: name, User: user, CPU: round1(cpu), MemRSS: rss, Cmdline: cmd})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MemRSS > out[j].MemRSS })
	return out
}
