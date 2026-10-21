package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hardynetworks/apex-rmm/internal/vsphere"
)

type vsCred struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type vsDeployJob struct {
	rowID, ref, name, os string
}

// deployVSphere installs the Apex agent into the selected VMs through VMware
// Tools guest operations. Guest credentials are used in memory only.
func (s *Server) deployVSphere(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var in struct {
		VMs      []string `json:"vms"`
		ClientID string   `json:"client_id"`
		SiteID   *string  `json:"site_id"`
		Windows  *vsCred  `json:"windows"`
		Linux    *vsCred  `json:"linux"`
		Force    bool     `json:"force"` // also deploy to VMs that already run an agent
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if len(in.VMs) == 0 || len(in.VMs) > 200 {
		fail(w, badRequest("select between 1 and 200 VMs"))
		return
	}
	pub := s.conf().PublicURL
	if u, err := url.Parse(pub); err != nil || u.Hostname() == "" || u.Hostname() == "localhost" || strings.HasPrefix(u.Hostname(), "127.") {
		fail(w, badRequest("set the server's Public URL in Settings first: the VMs must be able to reach it to download the agent"))
		return
	}
	c, err := s.vsLoad(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	clientID := in.ClientID
	if clientID == "" && c.ClientID != nil {
		clientID = *c.ClientID
	}
	if clientID == "" {
		fail(w, badRequest("choose the client the new devices belong to"))
		return
	}
	var clientOK bool
	_ = s.db.QueryRow(ctx, `SELECT true FROM clients WHERE id=$1`, clientID).Scan(&clientOK)
	if !clientOK {
		fail(w, badRequest("unknown client"))
		return
	}
	inv, _, err := s.loadInventory(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	if inv == nil {
		fail(w, badRequest("the inventory has not been read yet; refresh the connection first"))
		return
	}
	byRef := map[string]vsphere.VM{}
	for _, vm := range inv.VMs {
		byRef[vm.Ref] = vm
	}
	links := s.vsAgentLinks(ctx, inv.VMs)

	type skip struct{ ref, name, os, why string }
	var jobs []vsDeployJob
	var skips []skip
	seen := map[string]bool{}
	needWin, needLinux := false, false
	for _, ref := range in.VMs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		vm, ok := byRef[ref]
		switch {
		case !ok:
			skips = append(skips, skip{ref, ref, "", "not found in the inventory"})
		case vm.Template:
			skips = append(skips, skip{ref, vm.Name, vm.OSFamily, "it is a template"})
		case vm.PowerState != "poweredOn":
			skips = append(skips, skip{ref, vm.Name, vm.OSFamily, "the VM is not powered on"})
		case !vm.ToolsOK():
			skips = append(skips, skip{ref, vm.Name, vm.OSFamily, "VMware Tools is not running in the guest"})
		case vm.OSFamily != "windows" && vm.OSFamily != "linux":
			skips = append(skips, skip{ref, vm.Name, vm.OSFamily, "only Windows and Linux guests are supported"})
		default:
			if l, linked := links[ref]; linked && !in.Force {
				skips = append(skips, skip{ref, vm.Name, vm.OSFamily, "an agent is already installed (" + l.Name + ")"})
				continue
			}
			jobs = append(jobs, vsDeployJob{ref: ref, name: vm.Name, os: vm.OSFamily})
			if vm.OSFamily == "windows" {
				needWin = true
			} else {
				needLinux = true
			}
		}
	}
	if len(jobs) == 0 {
		reasons := make([]string, 0, len(skips))
		for _, k := range skips {
			reasons = append(reasons, k.name+": "+k.why)
		}
		fail(w, badRequest("nothing to deploy. "+strings.Join(reasons, "; ")))
		return
	}
	if needWin && (in.Windows == nil || in.Windows.Username == "" || in.Windows.Password == "") {
		fail(w, badRequest("enter a Windows administrator account for the Windows VMs"))
		return
	}
	if needLinux && (in.Linux == nil || in.Linux.Username == "" || in.Linux.Password == "") {
		fail(w, badRequest("enter a Linux account (root, or a user with passwordless sudo) for the Linux VMs"))
		return
	}

	u := userFrom(r)
	batch := randToken(9)
	var tokID, token string
	if err := s.db.QueryRow(ctx, `INSERT INTO enrollment_tokens (token, client_id, site_id, description, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4,now() + interval '2 hours',$5,$6) RETURNING id, token`,
		randToken(24), clientID, nilIfEmpty(in.SiteID), "vSphere deploy "+c.Name, len(jobs), u.Display()).Scan(&tokID, &token); err != nil {
		fail(w, badRequest("could not create an enrollment token: "+err.Error()))
		return
	}

	for i := range jobs {
		if err := s.db.QueryRow(ctx, `INSERT INTO vsphere_deploys (batch_id, connection_id, vm_ref, vm_name, os_family, status, created_by) VALUES ($1,$2,$3,$4,$5,'queued',$6) RETURNING id`,
			batch, id, jobs[i].ref, jobs[i].name, jobs[i].os, u.Display()).Scan(&jobs[i].rowID); err != nil {
			fail(w, err)
			return
		}
	}
	for _, k := range skips {
		_, _ = s.db.Exec(ctx, `INSERT INTO vsphere_deploys (batch_id, connection_id, vm_ref, vm_name, os_family, status, message, created_by, finished_at) VALUES ($1,$2,$3,$4,$5,'skipped',$6,$7,now())`,
			batch, id, k.ref, k.name, k.os, "Skipped: "+k.why, u.Display())
	}
	s.audit(ctx, u, r, "vsphere.deploy_started", "vsphere_connection", id, map[string]any{"connection": c.Name, "vms": len(jobs), "skipped": len(skips), "client_id": clientID})

	base := pub + "/install/" + token + "/"
	var win, lin vsphere.GuestAuth
	if in.Windows != nil {
		win = vsphere.GuestAuth{Username: in.Windows.Username, Password: in.Windows.Password}
	}
	if in.Linux != nil {
		lin = vsphere.GuestAuth{Username: in.Linux.Username, Password: in.Linux.Password}
	}
	go s.runVSphereDeploy(batch, c, tokID, base, jobs, win, lin, u)

	rows, _ := s.db.Maps(ctx, `SELECT id, vm_ref, vm_name, os_family, status, message, created_at FROM vsphere_deploys WHERE batch_id=$1 ORDER BY created_at, vm_name`, batch)
	writeJSON(w, 200, map[string]any{"batch_id": batch, "deploys": rows})
}

func nilIfEmpty(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

// runVSphereDeploy works through the VMs four at a time. The enrollment token
// is revoked at the end whatever happens.
func (s *Server) runVSphereDeploy(batch string, c *vsConn, tokID, installBase string, jobs []vsDeployJob, win, lin vsphere.GuestAuth, by *User) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	defer func() {
		_, _ = s.db.Exec(context.Background(), `UPDATE enrollment_tokens SET revoked=true WHERE id=$1`, tokID)
		_, _ = s.db.Exec(context.Background(), `UPDATE vsphere_deploys SET status='failed', message='did not finish', finished_at=now() WHERE batch_id=$1 AND status IN ('queued','running')`, batch)
	}()
	conn, err := s.vs.session(ctx, c)
	if err != nil {
		_, _ = s.db.Exec(ctx, `UPDATE vsphere_deploys SET status='failed', message=$2, finished_at=now() WHERE batch_id=$1 AND status='queued'`, batch, "cannot sign in to vSphere: "+err.Error())
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, j := range jobs {
		j := j
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.deployOne(ctx, conn, c, batch, installBase, j, win, lin, by)
		}()
	}
	wg.Wait()
}

func (s *Server) deployOne(ctx context.Context, conn *vsphere.Conn, c *vsConn, batch, installBase string, j vsDeployJob, win, lin vsphere.GuestAuth, by *User) {
	_, _ = s.db.Exec(ctx, `UPDATE vsphere_deploys SET status='running', started_at=now() WHERE id=$1`, j.rowID)
	auth, file := lin, "linux.sh"
	if j.os == "windows" {
		auth, file = win, "windows.ps1"
	}
	res, err := s.vs.install(ctx, conn, j.ref, j.os, auth, installBase+file, 10*time.Minute)
	status, msg, log := "success", "Installer finished. The device should appear in Devices within a minute.", ""
	var exit *int32
	if res != nil {
		log, exit = res.Log, &res.ExitCode
	}
	switch {
	case err != nil:
		status, msg = "failed", err.Error()
	case res != nil && res.ExitCode != 0:
		status, msg = "failed", fmt.Sprintf("the installer exited with code %d", res.ExitCode)
	}
	_, dbErr := s.db.Exec(context.Background(), `UPDATE vsphere_deploys SET status=$2, message=$3, log=$4, exit_code=$5, finished_at=now() WHERE id=$1`, j.rowID, status, msg, log, exit)
	if dbErr != nil {
		slog.Error("record deploy result", "err", dbErr)
	}
	s.audit(context.Background(), by, nil, "vsphere.deploy_vm", "vsphere_vm", j.ref, map[string]any{"connection": c.Name, "vm": j.name, "os": j.os, "status": status})
}

func (s *Server) listVSphereDeploys(w http.ResponseWriter, r *http.Request) {
	args := []any{chi.URLParam(r, "id")}
	where := ""
	if b := r.URL.Query().Get("batch"); b != "" {
		args = append(args, b)
		where = " AND batch_id=$2"
	}
	rows, err := s.db.Maps(r.Context(), `SELECT id, batch_id, vm_ref, vm_name, os_family, status, message, log, exit_code, created_by, created_at, started_at, finished_at
		FROM vsphere_deploys WHERE connection_id=$1`+where+` ORDER BY created_at DESC, vm_name LIMIT 300`, args...)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
