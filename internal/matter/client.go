// Package matter talks to a Matter controller (matterjs-server, or the older python-matter-server,
// which share a WebSocket API) to read and switch Matter smart plugs.
//
// The controller holds the Matter fabric: it commissions devices and keeps a live copy of every
// node's attributes. This client subscribes to that ("start_listening") and keeps the same copy,
// updated by attribute events, so reads never wait on the network.
package matter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Node is one commissioned Matter device as the controller reports it. Attributes are keyed
// "endpoint/cluster/attribute" (decimal ids), e.g. "1/6/0" is endpoint 1's OnOff state.
type Node struct {
	NodeID     int64          `json:"node_id"`
	Available  bool           `json:"available"`
	Attributes map[string]any `json:"attributes"`
}

// Cluster and attribute ids used here (Matter 1.3 specification).
const (
	clusterBasic  = 40  // Basic Information: 1 VendorName, 3 ProductName, 5 NodeLabel, 15 SerialNumber
	clusterOnOff  = 6   // 0 OnOff
	clusterPower  = 144 // Electrical Power Measurement: 4 Voltage (mV), 5 ActiveCurrent (mA), 8 ActivePower (mW)
	clusterEnergy = 145 // Electrical Energy Measurement: 1 CumulativeEnergyImported {energy: mWh}
)

// Plug is the part of a node that matters for a smart plug.
type Plug struct {
	NodeID    int64    `json:"node_id"`
	Available bool     `json:"available"`
	Vendor    string   `json:"vendor"`
	Product   string   `json:"product"`
	Label     string   `json:"label"`
	Serial    string   `json:"serial"`
	Endpoint  int      `json:"endpoint"` // the endpoint with the OnOff cluster (0 if none)
	On        *bool    `json:"on"`
	Watts     *float64 `json:"watts"`      // nil: the plug doesn't measure power
	Volts     *float64 `json:"volts"`      // nil if not reported
	Amps      *float64 `json:"amps"`       // nil if not reported
	EnergyKWh *float64 `json:"energy_kwh"` // the plug's own energy counter, nil if none
}

// AsPlug picks the plug details out of a node's attributes.
func (n *Node) AsPlug() Plug {
	p := Plug{NodeID: n.NodeID, Available: n.Available}
	str := func(k string) string { s, _ := n.Attributes[k].(string); return strings.TrimSpace(s) }
	p.Vendor, p.Product, p.Label, p.Serial = str("0/40/1"), str("0/40/3"), str("0/40/5"), str("0/40/15")
	// the lowest non-root endpoint with an OnOff attribute is the outlet
	p.Endpoint = n.firstEndpoint(clusterOnOff, 0)
	if p.Endpoint > 0 {
		if v, ok := n.Attributes[attrKey(p.Endpoint, clusterOnOff, 0)].(bool); ok {
			p.On = &v
		}
	}
	if ep := n.firstEndpoint(clusterPower, 8); ep >= 0 {
		if mw, ok := number(n.Attributes[attrKey(ep, clusterPower, 8)]); ok {
			w := mw / 1000
			p.Watts = &w
		}
		if mv, ok := number(n.Attributes[attrKey(ep, clusterPower, 4)]); ok {
			v := mv / 1000
			p.Volts = &v
		}
		if ma, ok := number(n.Attributes[attrKey(ep, clusterPower, 5)]); ok {
			a := ma / 1000
			p.Amps = &a
		}
	}
	if ep := n.firstEndpoint(clusterEnergy, 1); ep >= 0 {
		if mwh, ok := energyOf(n.Attributes[attrKey(ep, clusterEnergy, 1)]); ok {
			k := mwh / 1e6
			p.EnergyKWh = &k
		}
	}
	return p
}

func attrKey(ep, cluster, attr int) string { return fmt.Sprintf("%d/%d/%d", ep, cluster, attr) }

// firstEndpoint returns the lowest endpoint (> 0 when possible) that has cluster/attr, or -1.
func (n *Node) firstEndpoint(cluster, attr int) int {
	best := -1
	for k := range n.Attributes {
		parts := strings.Split(k, "/")
		if len(parts) != 3 || parts[1] != strconv.Itoa(cluster) || parts[2] != strconv.Itoa(attr) {
			continue
		}
		ep, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		if best < 0 || (ep > 0 && (best == 0 || ep < best)) {
			best = ep
		}
	}
	return best
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

// energyOf reads an EnergyMeasurementStruct: the controller sends field names ("energy") or ids ("0").
func energyOf(v any) (float64, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	for _, k := range []string{"energy", "0"} {
		if f, ok := number(m[k]); ok {
			return f, true
		}
	}
	return 0, false
}

// ---- client ----

// Status of the connection to the controller.
type Status struct {
	State string `json:"state"` // off, connecting, connected, error
	Error string `json:"error,omitempty"`
	Since string `json:"since,omitempty"`
}

type response struct {
	Result    json.RawMessage `json:"result"`
	ErrorCode *int            `json:"error_code"`
	Details   string          `json:"details"`
}

// Client keeps a connection to one controller.
type Client struct {
	url    string
	mu     sync.Mutex
	conn   *websocket.Conn
	nodes  map[int64]*Node
	wait   map[string]chan response
	status Status
	seq    atomic.Int64
	wmu    sync.Mutex // one writer at a time
	// OnUpdate is called (outside locks) after a node changes.
	OnUpdate func(nodeID int64)
}

// New returns a client for the controller's WebSocket URL (e.g. ws://host:5580/ws).
func New(url string) *Client {
	return &Client{url: url, nodes: map[int64]*Node{}, wait: map[string]chan response{}, status: Status{State: "connecting"}}
}

func (c *Client) URL() string { return c.url }

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Client) setStatus(state, err string) {
	c.mu.Lock()
	if c.status.State != state || c.status.Error != err {
		c.status = Status{State: state, Error: err, Since: time.Now().UTC().Format(time.RFC3339)}
	}
	c.mu.Unlock()
}

// Plugs returns every node as a plug, ordered by node id.
func (c *Client) Plugs() []Plug {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Plug, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, n.AsPlug())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Plug returns one node as a plug.
func (c *Client) Plug(id int64) (Plug, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.nodes[id]
	if n == nil {
		return Plug{}, false
	}
	return n.AsPlug(), true
}

// Run connects and reconnects until ctx ends.
func (c *Client) Run(ctx context.Context) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			break
		}
		msg := "disconnected"
		if err != nil {
			msg = err.Error()
		}
		c.setStatus("error", msg)
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	c.setStatus("off", "")
}

func (c *Client) session(ctx context.Context) error {
	c.setStatus("connecting", "")
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.DialContext(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("cannot reach the Matter server at %s: %v", c.url, err)
	}
	conn.SetReadLimit(64 << 20)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		for id, ch := range c.wait {
			close(ch)
			delete(c.wait, id)
		}
		c.mu.Unlock()
		conn.Close()
	}()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(conn) }()

	// subscribe: the result is every node, then events keep them current
	res, err := c.call(ctx, "start_listening", nil, 60*time.Second)
	if err != nil {
		return err
	}
	var nodes []*Node
	if err := json.Unmarshal(res, &nodes); err != nil {
		return fmt.Errorf("unexpected node list from the Matter server: %v", err)
	}
	c.mu.Lock()
	c.nodes = map[int64]*Node{}
	for _, n := range nodes {
		if n.Attributes == nil {
			n.Attributes = map[string]any{}
		}
		c.nodes[n.NodeID] = n
	}
	c.mu.Unlock()
	c.setStatus("connected", "")
	if c.OnUpdate != nil {
		for _, n := range nodes {
			c.OnUpdate(n.NodeID)
		}
	}
	return <-readErr
}

func (c *Client) readLoop(conn *websocket.Conn) error {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var m struct {
			MessageID string          `json:"message_id"`
			Event     string          `json:"event"`
			Data      json.RawMessage `json:"data"`
			response
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.MessageID != "" {
			c.mu.Lock()
			ch := c.wait[m.MessageID]
			delete(c.wait, m.MessageID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m.response
			}
			continue
		}
		if m.Event != "" {
			c.event(m.Event, m.Data)
		}
	}
}

func (c *Client) event(kind string, data json.RawMessage) {
	var changed int64 = -1
	switch kind {
	case "node_added", "node_updated":
		var n Node
		if json.Unmarshal(data, &n) == nil && n.NodeID != 0 {
			if n.Attributes == nil {
				n.Attributes = map[string]any{}
			}
			c.mu.Lock()
			c.nodes[n.NodeID] = &n
			c.mu.Unlock()
			changed = n.NodeID
		}
	case "node_removed":
		var id int64
		if json.Unmarshal(data, &id) == nil {
			c.mu.Lock()
			delete(c.nodes, id)
			c.mu.Unlock()
			changed = id
		}
	case "attribute_updated":
		var d []json.RawMessage // [node_id, "ep/cluster/attr", value]
		if json.Unmarshal(data, &d) == nil && len(d) == 3 {
			var id int64
			var path string
			var val any
			if json.Unmarshal(d[0], &id) == nil && json.Unmarshal(d[1], &path) == nil && json.Unmarshal(d[2], &val) == nil {
				c.mu.Lock()
				if n := c.nodes[id]; n != nil {
					n.Attributes[path] = val
				}
				c.mu.Unlock()
				changed = id
			}
		}
	case "node_available", "node_unavailable":
		var id int64
		if json.Unmarshal(data, &id) == nil {
			c.mu.Lock()
			if n := c.nodes[id]; n != nil {
				n.Available = kind == "node_available"
			}
			c.mu.Unlock()
			changed = id
		}
	}
	if changed >= 0 && c.OnUpdate != nil {
		c.OnUpdate(changed)
	}
}

// ErrNotConnected is returned by commands while the controller can't be reached.
var ErrNotConnected = errors.New("not connected to the Matter server")

func (c *Client) call(ctx context.Context, command string, args any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	id := strconv.FormatInt(c.seq.Add(1), 10)
	ch := make(chan response, 1)
	c.wait[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"message_id": id, "command": command}
	if args != nil {
		msg["args"] = args
	}
	c.wmu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := conn.WriteJSON(msg)
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if r.ErrorCode != nil {
			d := r.Details
			if d == "" {
				d = "error " + strconv.Itoa(*r.ErrorCode)
			}
			return nil, fmt.Errorf("Matter server: %s", d)
		}
		return r.Result, nil
	case <-t.C:
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("the Matter server did not answer %s in time", command)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Commission adds a device with its pairing code: the 11/21-digit manual code or the "MT:..." QR
// payload. The device must already be on the network (shared from another app with "pairing
// mode" / multi-admin, or a Matter-over-Wi-Fi plug in pairing mode on the same LAN).
func (c *Client) Commission(ctx context.Context, code string) (Plug, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(strings.ToUpper(code), "MT:") {
		code = strings.NewReplacer("-", "", " ", "").Replace(code)
	}
	res, err := c.call(ctx, "commission_with_code", map[string]any{"code": code, "network_only": true}, 3*time.Minute)
	if err != nil {
		return Plug{}, err
	}
	var n Node
	if err := json.Unmarshal(res, &n); err != nil || n.NodeID == 0 {
		return Plug{}, fmt.Errorf("unexpected answer from the Matter server")
	}
	if n.Attributes == nil {
		n.Attributes = map[string]any{}
	}
	c.mu.Lock()
	c.nodes[n.NodeID] = &n
	c.mu.Unlock()
	return n.AsPlug(), nil
}

// SetOn switches a plug's outlet.
func (c *Client) SetOn(ctx context.Context, id int64, on bool) error {
	p, ok := c.Plug(id)
	if !ok {
		return fmt.Errorf("no Matter node %d", id)
	}
	if p.Endpoint <= 0 {
		return fmt.Errorf("this device has no on/off switch")
	}
	cmd := "Off"
	if on {
		cmd = "On"
	}
	_, err := c.call(ctx, "device_command", map[string]any{
		"node_id": id, "endpoint_id": p.Endpoint, "cluster_id": clusterOnOff, "command_name": cmd, "payload": map[string]any{},
	}, 30*time.Second)
	return err
}

// Remove decommissions a node from the controller's fabric.
func (c *Client) Remove(ctx context.Context, id int64) error {
	_, err := c.call(ctx, "remove_node", map[string]any{"node_id": id}, time.Minute)
	if err == nil {
		c.mu.Lock()
		delete(c.nodes, id)
		c.mu.Unlock()
	}
	return err
}
