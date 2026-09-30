package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
)

// Server is the Apex RMM API + dashboard server.
type Server struct {
	base      *Config                // environment
	cfgp      atomic.Pointer[Config] // effective (environment + Settings)
	box       *secretBox
	setupCode string
	db        *DB
	auth      *Auth
	hub       *Hub
}

// conf returns the current effective configuration.
func (s *Server) conf() *Config { return s.cfgp.Load() }

// New builds the server (connects to DB, loads settings, starts OIDC discovery).
func New(ctx context.Context, cfg *Config) (*Server, error) {
	db, err := OpenDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	s := &Server{base: cfg, db: db, box: newSecretBox(cfg.AppKeyFile)}
	s.cfgp.Store(cfg.withSettings(SystemSettings{}, ""))
	if err := s.reloadConfig(ctx); err != nil {
		return nil, err
	}
	s.hub = newHub(s)
	s.auth = newAuth(s)
	go s.auth.reloadWithRetry(ctx)
	if s.setupNeeded(ctx) {
		s.setupCode = strings.ToUpper(randPassword(8))
		fmt.Printf("\n==============================================================\n"+
			"  Apex RMM is not set up yet. Open the dashboard in a browser\n"+
			"  and enter this setup code:\n\n"+
			"      setup code: %s\n"+
			"==============================================================\n\n", s.setupCode)
	}
	// Nobody is connected at startup.
	_, _ = db.Exec(ctx, `UPDATE devices SET online=false`)
	_, _ = db.Exec(ctx, `UPDATE job_results SET status='pending' WHERE status='running' AND started_at > now() - interval '1 hour'`)
	s.seed(ctx)
	return s, nil
}

// Run starts HTTP and background workers until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go s.alertLoop(ctx)
	go s.schedulerLoop(ctx)
	go s.cleanupLoop(ctx)
	srv := &http.Server{Addr: s.conf().Listen, Handler: s.routes(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("Apex RMM listening", "addr", s.conf().Listen, "public_url", s.conf().PublicURL, "sso", s.conf().OIDCEnabled())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "agents": s.hub.OnlineCount()})
	})

	// SSO
	r.Get("/auth/login", s.auth.handleLogin)
	r.Get("/auth/callback", s.auth.handleCallback)

	// Agent endpoints (authenticated by enrollment token / device secret)
	r.Post("/api/agent/enroll", s.handleEnroll)
	r.Get("/api/agent/ws", s.handleAgentWS)
	r.Get("/api/agent/desktop", s.handleDesktopHelper)
	r.Get("/api/agent/version", s.handleAgentVersion)
	r.Get("/download/agent/{os}/{arch}", s.handleAgentDownload)
	r.Get("/install/{token}/{file}", s.handleInstaller)

	r.Route("/api", func(r chi.Router) {
		r.Get("/auth/config", s.auth.handleConfig)
		r.Post("/auth/local", s.auth.handleLocalLogin)
		r.Get("/setup", s.handleSetupStatus)
		r.Post("/setup", s.handleSetup)

		r.Group(func(r chi.Router) {
			r.Use(s.auth.requireUser)
			r.Get("/auth/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, userFrom(r)) })
			r.Post("/auth/logout", s.auth.handleLogout)
			r.Post("/auth/password", s.changePassword)

			// read-only (viewer+)
			r.Get("/dashboard", s.handleDashboard)
			r.Get("/clients", s.listClients)
			r.Get("/devices", s.listDevices)
			r.Get("/devices/{id}", s.getDevice)
			r.Get("/devices/{id}/metrics", s.deviceMetrics)
			r.Get("/devices/{id}/jobs", s.deviceJobs)
			r.Get("/devices/{id}/alerts", s.deviceAlerts)
			r.Get("/scripts", s.listScripts)
			r.Get("/scripts/{id}", s.getScript)
			r.Get("/jobs", s.listJobs)
			r.Get("/jobs/{id}", s.getJob)
			r.Get("/schedules", s.listSchedules)
			r.Get("/alerts", s.listAlerts)
			r.Get("/alert-policies", s.listPolicies)
			r.Get("/tickets", s.listTickets)
			r.Get("/tickets/{id}", s.getTicket)
			r.Get("/users", s.listUsers)

			// technician+
			r.Group(func(r chi.Router) {
				r.Use(requireRole("technician"))
				r.Patch("/devices/{id}", s.updateDevice)
				r.Get("/devices/{id}/processes", s.deviceProcesses)
				r.Post("/devices/{id}/processes/{pid}/kill", s.killProcess)
				r.Get("/devices/{id}/services", s.deviceServices)
				r.Post("/devices/{id}/services/{name}/{action}", s.serviceAction)
				r.Get("/devices/{id}/software", s.deviceSoftware)
				r.Post("/devices/{id}/power", s.devicePower)
				r.Post("/devices/{id}/refresh", s.deviceRefresh)
				r.Post("/devices/{id}/update-agent", s.deviceUpdateAgent)
				r.Post("/devices/{id}/desktop", s.startDesktop)
				r.Get("/devices/{id}/rustdesk", s.getRustDesk)
				r.Post("/devices/{id}/rustdesk", s.provisionRustDesk)
				r.Get("/ws/desktop/{sid}", s.wsDesktop)
				r.Get("/ws/terminal/{id}", s.wsTerminal)

				r.Post("/scripts", s.saveScript)
				r.Put("/scripts/{id}", s.saveScript)
				r.Delete("/scripts/{id}", s.deleteScript)
				r.Post("/jobs", s.createJob)
				r.Post("/schedules", s.saveSchedule)
				r.Put("/schedules/{id}", s.saveSchedule)
				r.Delete("/schedules/{id}", s.deleteSchedule)
				r.Post("/schedules/{id}/run", s.runScheduleNow)

				r.Post("/alerts/{id}/ack", s.ackAlert)
				r.Post("/alerts/{id}/resolve", s.resolveAlert)
				r.Post("/alerts/{id}/ticket", s.alertToTicket)

				r.Post("/tickets", s.createTicket)
				r.Patch("/tickets/{id}", s.updateTicket)
				r.Post("/tickets/{id}/comments", s.addComment)
			})

			// admin
			r.Group(func(r chi.Router) {
				r.Use(requireRole("admin"))
				r.Delete("/devices/{id}", s.deleteDevice)
				r.Post("/clients", s.saveClient)
				r.Put("/clients/{id}", s.saveClient)
				r.Delete("/clients/{id}", s.deleteClient)
				r.Post("/clients/{id}/sites", s.addSite)
				r.Delete("/sites/{id}", s.deleteSite)
				r.Get("/enrollment-tokens", s.listTokens)
				r.Post("/enrollment-tokens", s.createToken)
				r.Delete("/enrollment-tokens/{id}", s.revokeToken)
				r.Post("/alert-policies", s.savePolicy)
				r.Put("/alert-policies/{id}", s.savePolicy)
				r.Delete("/alert-policies/{id}", s.deletePolicy)
				r.Patch("/users/{id}", s.updateUser)
				r.Get("/settings", s.getSettings)
				r.Put("/settings", s.putSettings)
				r.Post("/settings/test-webhook", s.testWebhook)
				r.Get("/settings/system", s.getSystemSettings)
				r.Put("/settings/system", s.putSystemSettings)
				r.Post("/settings/test-oidc", s.testOIDC)
				r.Get("/audit", s.listAudit)
				r.Delete("/tickets/{id}", s.deleteTicket)
			})
		})
	})

	r.NotFound(s.serveSPA)
	return r
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// serveSPA serves the built dashboard, falling back to index.html.
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, 404, "not found")
		return
	}
	p := filepath.Join(s.conf().WebDir, filepath.Clean("/"+r.URL.Path))
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(w, r, p)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filepath.Join(s.conf().WebDir, "index.html"))
}

var browserUpgrader = websocket.Upgrader{ReadBufferSize: 16 << 10, WriteBufferSize: 64 << 10}

func (s *Server) upgradeBrowser(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	up := browserUpgrader
	up.CheckOrigin = s.checkOrigin
	return up.Upgrade(w, r, nil)
}

// audit records an action. u may be nil for system/agent actions.
func (s *Server) audit(ctx context.Context, u *User, r *http.Request, action, targetType, targetID string, details map[string]any) {
	var uid *string
	name := "system"
	if u != nil {
		uid = &u.ID
		name = u.Display()
	}
	ip := ""
	if r != nil {
		ip = s.clientIP(r)
	}
	if details == nil {
		details = map[string]any{}
	}
	b, _ := json.Marshal(details)
	if _, err := s.db.Exec(ctx, `INSERT INTO audit_log (user_id, user_name, action, target_type, target_id, details, ip) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		uid, name, action, targetType, targetID, b, ip); err != nil {
		slog.Error("audit", "err", err)
	}
}

func (s *Server) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		_, _ = s.db.Exec(ctx, `DELETE FROM device_metrics WHERE ts < now() - make_interval(days => $1)`, s.conf().MetricsRetentionDays)
		_, _ = s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
		_, _ = s.db.Exec(ctx, `UPDATE job_results SET status='timeout', finished_at=now() WHERE status='running' AND started_at < now() - interval '6 hours'`)
		_, _ = s.db.Exec(ctx, `DELETE FROM audit_log WHERE created_at < now() - interval '365 days'`)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
