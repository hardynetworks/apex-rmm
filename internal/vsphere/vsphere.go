// Package vsphere talks to a vCenter Server or a standalone ESXi host through
// the vSphere API (govmomi). It is agentless: nothing is installed on the
// hypervisor. Apex RMM uses it to inventory hosts, VMs and datastores, to
// raise alerts, to run VM power actions and to push the Apex agent into guests
// through VMware Tools (guest operations).
package vsphere

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
)

// Config describes how to reach and sign in to a vCenter / ESXi endpoint.
type Config struct {
	URL        string // host name, host:port, or full https://host/sdk URL
	Username   string
	Password   string
	Insecure   bool   // skip TLS verification entirely
	Thumbprint string // trusted certificate thumbprint (SHA-1 or SHA-256, colon hex)
}

// CertError is returned when the server's certificate is not trusted. It
// carries the thumbprint so the operator can choose to trust it.
type CertError struct {
	Host       string
	Thumbprint string
	Err        error
}

func (e *CertError) Error() string {
	return fmt.Sprintf("the certificate presented by %s is not trusted (%v)", e.Host, e.Err)
}
func (e *CertError) Unwrap() error { return e.Err }

// ParseURL turns "vcenter.lab", "vcenter.lab:8443" or a full URL into the
// https://host/sdk endpoint and returns the host:port part as well.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("vCenter / ESXi address is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not a valid address", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("address must start with https://")
	}
	u.Path = "/sdk"
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u, nil
}

// Conn is a signed-in vSphere session. It is safe for concurrent use.
type Conn struct {
	cfg Config
	url *url.URL
	c   *govmomi.Client
}

// Connect signs in. Untrusted certificates yield a *CertError.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	u, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.Username == "" {
		return nil, errors.New("username is required")
	}
	sc := soap.NewClient(u, cfg.Insecure)
	if cfg.Thumbprint != "" {
		sc.SetThumbprint(u.Host, normThumb(cfg.Thumbprint))
	}
	sc.UserAgent = "apex-rmm"
	vc, err := vim25.NewClient(ctx, sc)
	if err != nil {
		if soap.IsCertificateUntrusted(err) {
			return nil, &CertError{Host: u.Host, Thumbprint: fetchThumb(ctx, u.Host), Err: err}
		}
		return nil, fmt.Errorf("cannot reach %s: %w", u.Host, err)
	}
	gc := &govmomi.Client{Client: vc, SessionManager: session.NewManager(vc)}
	if err := gc.Login(ctx, url.UserPassword(cfg.Username, cfg.Password)); err != nil {
		return nil, fmt.Errorf("sign-in failed: %w", cleanErr(err))
	}
	// Keep the session alive between polls (re-login is handled by KeepAlive).
	gc.Client.RoundTripper = session.KeepAlive(gc.Client.RoundTripper, 10*time.Minute)
	return &Conn{cfg: cfg, url: u, c: gc}, nil
}

// Close signs out.
func (c *Conn) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.c.Logout(ctx)
}

// Client exposes the underlying vim25 client (used by tests).
func (c *Conn) Client() *vim25.Client { return c.c.Client }

// About describes the endpoint.
type About struct {
	Name         string `json:"name"`
	FullName     string `json:"full_name"`
	Version      string `json:"version"`
	Build        string `json:"build"`
	APIType      string `json:"api_type"` // VirtualCenter | HostAgent
	InstanceUUID string `json:"instance_uuid"`
}

func (c *Conn) About() About {
	a := c.c.ServiceContent.About
	return About{Name: a.Name, FullName: a.FullName, Version: a.Version, Build: a.Build, APIType: a.ApiType, InstanceUUID: a.InstanceUuid}
}

// IsVCenter reports whether the endpoint is a vCenter Server (rather than a lone ESXi host).
func (c *Conn) IsVCenter() bool { return c.c.ServiceContent.About.ApiType == "VirtualCenter" }

// Probe signs in and counts objects; used by "Test connection".
type ProbeResult struct {
	About      About `json:"about"`
	Hosts      int   `json:"hosts"`
	VMs        int   `json:"vms"`
	Datastores int   `json:"datastores"`
}

func Probe(ctx context.Context, cfg Config) (*ProbeResult, error) {
	c, err := Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	m := view.NewManager(c.c.Client)
	root := c.c.ServiceContent.RootFolder
	res := &ProbeResult{About: c.About()}
	if v, err := m.CreateContainerView(ctx, root, []string{"HostSystem"}, true); err == nil {
		var l []mo.HostSystem
		if v.Retrieve(ctx, []string{"HostSystem"}, []string{"name"}, &l) == nil {
			res.Hosts = len(l)
		}
		_ = v.Destroy(ctx)
	}
	if v, err := m.CreateContainerView(ctx, root, []string{"VirtualMachine"}, true); err == nil {
		var l []mo.VirtualMachine
		if v.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name"}, &l) == nil {
			res.VMs = len(l)
		}
		_ = v.Destroy(ctx)
	}
	if v, err := m.CreateContainerView(ctx, root, []string{"Datastore"}, true); err == nil {
		var l []mo.Datastore
		if v.Retrieve(ctx, []string{"Datastore"}, []string{"name"}, &l) == nil {
			res.Datastores = len(l)
		}
		_ = v.Destroy(ctx)
	}
	return res, nil
}

// ---- helpers ----

func normThumb(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	t = strings.ReplaceAll(t, " ", "")
	if !strings.Contains(t, ":") && (len(t) == 40 || len(t) == 64) {
		var parts []string
		for i := 0; i < len(t); i += 2 {
			parts = append(parts, t[i:i+2])
		}
		t = strings.Join(parts, ":")
	}
	return t
}

// fetchThumb returns the SHA-1 thumbprint of the certificate a server presents.
func fetchThumb(ctx context.Context, hostport string) string {
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		hostport = net.JoinHostPort(hostport, "443")
	}
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", hostport)
	if err != nil {
		return ""
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return ""
	}
	sum := sha1.Sum(certs[0].Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// cleanErr turns SOAP faults into short operator-friendly messages.
func cleanErr(err error) error {
	if err == nil {
		return nil
	}
	if soap.IsSoapFault(err) {
		if f := soap.ToSoapFault(err); f != nil && f.String != "" {
			return errors.New(f.String)
		}
	}
	return err
}
