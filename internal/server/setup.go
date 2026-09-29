package server

import (
	"context"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ---- system settings (dashboard-editable configuration) ----

func (s *Server) loadSystem(ctx context.Context) SystemSettings {
	var ss SystemSettings
	s.db.Setting(ctx, "system", &ss)
	return ss
}

// reloadConfig rebuilds the effective config from environment + stored settings.
func (s *Server) reloadConfig(ctx context.Context) error {
	ss := s.loadSystem(ctx)
	secret, err := s.box.open(ss.OIDCClientSecret)
	if err != nil {
		return err
	}
	s.cfgp.Store(s.base.withSettings(ss, secret))
	return nil
}

// setupNeeded is true on a fresh install that nobody can sign in to yet.
func (s *Server) setupNeeded(ctx context.Context) bool {
	if s.base.LocalAdminPassword != "" || s.loadSystem(ctx).SetupComplete {
		return false
	}
	if s.base.Locked["oidc_issuer"] && s.base.Locked["oidc_client_id"] {
		return false // SSO fully configured through the environment
	}
	var n int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n == 0
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"needed": s.setupNeeded(r.Context()), "public_url_locked": s.base.Locked["public_url"], "public_url": s.base.PublicURL})
}

// handleSetup creates the first administrator and the basic settings.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Code        string `json:"code"`
		Email       string `json:"email"`
		Name        string `json:"name"`
		Password    string `json:"password"`
		PublicURL   string `json:"public_url"`
		CompanyName string `json:"company_name"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if !s.setupNeeded(ctx) {
		writeErr(w, http.StatusConflict, "setup has already been completed")
		return
	}
	if s.setupCode == "" || !constEq(strings.ToUpper(strings.TrimSpace(in.Code)), strings.ToUpper(s.setupCode)) {
		s.audit(ctx, nil, r, "setup.bad_code", "system", "", nil)
		writeErr(w, http.StatusForbidden, "wrong setup code - find it in the server log (docker compose logs hardy)")
		return
	}
	in.Email = strings.TrimSpace(in.Email)
	if !strings.Contains(in.Email, "@") {
		fail(w, badRequest("enter a valid email address"))
		return
	}
	if len(in.Password) < 10 {
		fail(w, badRequest("password must be at least 10 characters"))
		return
	}
	in.PublicURL = strings.TrimRight(strings.TrimSpace(in.PublicURL), "/")
	if !s.base.Locked["public_url"] {
		if err := validPublicURL(in.PublicURL); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, err)
		return
	}
	var uid string
	if err := s.db.QueryRow(ctx, `INSERT INTO users (email, name, username, role, password_hash) VALUES ($1,$2,$3,'admin',$4) RETURNING id`,
		in.Email, strings.TrimSpace(in.Name), strings.Split(in.Email, "@")[0], string(hash)).Scan(&uid); err != nil {
		fail(w, err)
		return
	}
	ss := s.loadSystem(ctx)
	ss.PublicURL = in.PublicURL
	ss.CompanyName = strings.TrimSpace(in.CompanyName)
	ss.SetupComplete = true
	if err := s.db.SetSetting(ctx, "system", ss); err != nil {
		fail(w, err)
		return
	}
	_ = s.reloadConfig(ctx)
	// rename the default client created before setup
	if ss.CompanyName != "" {
		_, _ = s.db.Exec(ctx, `UPDATE clients SET name=$1 WHERE name='Hardy RMM' AND NOT EXISTS (SELECT 1 FROM clients WHERE name=$1)`, ss.CompanyName)
	}
	s.setupCode = ""
	if err := s.auth.startSession(w, r, uid, ""); err != nil {
		fail(w, err)
		return
	}
	s.audit(ctx, &User{ID: uid, Email: in.Email, Name: in.Name}, r, "setup.completed", "system", "", map[string]any{"public_url": in.PublicURL})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- Settings → System ----

type systemView struct {
	Values        map[string]any  `json:"values"`
	Locked        map[string]bool `json:"locked"`
	SecretSet     bool            `json:"oidc_client_secret_set"`
	SSOActive     bool            `json:"sso_active"`
	SSOError      string          `json:"sso_error"`
	RedirectURI   string          `json:"redirect_uri"`
	KeyDetected   string          `json:"rustdesk_key_detected"`
	Encrypted     bool            `json:"secrets_encrypted"`
	LocalAdminEnv bool            `json:"local_admin_env"`
}

func (s *Server) systemView() systemView {
	c := s.conf()
	active, errMsg := s.auth.Status()
	return systemView{
		Values: map[string]any{
			"public_url":         c.PublicURL,
			"company_name":       c.CompanyName,
			"oidc_issuer":        c.OIDCIssuer,
			"oidc_client_id":     c.OIDCClientID,
			"oidc_admin_groups":  strings.Join(c.OIDCAdminGroups, ", "),
			"oidc_tech_groups":   strings.Join(c.OIDCTechGroups, ", "),
			"oidc_viewer_groups": strings.Join(c.OIDCViewerGroups, ", "),
			"oidc_default_role":  c.OIDCDefaultRole,
			"oidc_logout_url":    c.OIDCLogoutURL,
			"rustdesk_host":      c.RustDeskHost,
			"rustdesk_relay":     relayValue(c),
			"rustdesk_key":       c.RustDeskKey,
		},
		Locked:        s.base.Locked,
		SecretSet:     c.OIDCClientSecret != "",
		SSOActive:     active,
		SSOError:      errMsg,
		RedirectURI:   c.PublicURL + "/auth/callback",
		KeyDetected:   readSecretFile(c.RustDeskKeyFile),
		Encrypted:     s.box.aead != nil,
		LocalAdminEnv: s.base.LocalAdminPassword != "",
	}
}

func (s *Server) getSystemSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.systemView())
}

type systemIn struct {
	PublicURL        string `json:"public_url"`
	CompanyName      string `json:"company_name"`
	OIDCIssuer       string `json:"oidc_issuer"`
	OIDCClientID     string `json:"oidc_client_id"`
	OIDCClientSecret string `json:"oidc_client_secret"` // empty = keep current
	ClearSecret      bool   `json:"clear_oidc_client_secret"`
	OIDCAdminGroups  string `json:"oidc_admin_groups"`
	OIDCTechGroups   string `json:"oidc_tech_groups"`
	OIDCViewerGroups string `json:"oidc_viewer_groups"`
	OIDCDefaultRole  string `json:"oidc_default_role"`
	OIDCLogoutURL    string `json:"oidc_logout_url"`
	RustDeskHost     string `json:"rustdesk_host"`
	RustDeskRelay    string `json:"rustdesk_relay"`
	RustDeskKey      string `json:"rustdesk_key"`
}

func (s *Server) putSystemSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in systemIn
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	in.PublicURL = strings.TrimRight(strings.TrimSpace(in.PublicURL), "/")
	if in.PublicURL != "" {
		if err := validPublicURL(in.PublicURL); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
	}
	if in.OIDCDefaultRole != "" && !validRole(in.OIDCDefaultRole) {
		fail(w, badRequest("default role must be admin, technician, viewer or empty"))
		return
	}
	in.OIDCIssuer = strings.TrimSpace(in.OIDCIssuer)
	if in.OIDCIssuer != "" && !strings.HasPrefix(in.OIDCIssuer, "https://") && !strings.HasPrefix(in.OIDCIssuer, "http://") {
		fail(w, badRequest("the issuer URL must start with https://"))
		return
	}
	u := userFrom(r)
	// Changing SSO must not lock the admin out: require a working local login
	// or an unchanged/verified provider.
	ss := s.loadSystem(ctx)
	prev := ss
	ss.PublicURL = in.PublicURL
	ss.CompanyName = strings.TrimSpace(in.CompanyName)
	ss.OIDCIssuer = in.OIDCIssuer
	ss.OIDCClientID = strings.TrimSpace(in.OIDCClientID)
	if in.ClearSecret {
		ss.OIDCClientSecret = ""
	} else if in.OIDCClientSecret != "" {
		ss.OIDCClientSecret = s.box.seal(in.OIDCClientSecret)
	}
	ss.OIDCAdminGroups = splitList(in.OIDCAdminGroups)
	ss.OIDCTechGroups = splitList(in.OIDCTechGroups)
	ss.OIDCViewerGroups = splitList(in.OIDCViewerGroups)
	ss.OIDCDefaultRole = in.OIDCDefaultRole
	ss.OIDCLogoutURL = strings.TrimSpace(in.OIDCLogoutURL)
	ss.RustDeskHost = strings.TrimSpace(in.RustDeskHost)
	ss.RustDeskRelay = strings.TrimSpace(in.RustDeskRelay)
	ss.RustDeskKey = strings.TrimSpace(in.RustDeskKey)
	ss.SetupComplete = true
	if err := s.db.SetSetting(ctx, "system", ss); err != nil {
		fail(w, err)
		return
	}
	if err := s.reloadConfig(ctx); err != nil {
		_ = s.db.SetSetting(ctx, "system", prev)
		_ = s.reloadConfig(ctx)
		fail(w, badRequest(err.Error()))
		return
	}
	ssoErr := ""
	if err := s.auth.reload(ctx); err != nil {
		ssoErr = err.Error()
	}
	s.audit(ctx, u, r, "settings.system_updated", "settings", "system", map[string]any{
		"public_url": ss.PublicURL, "oidc_issuer": ss.OIDCIssuer, "rustdesk_host": ss.RustDeskHost, "secret_changed": in.OIDCClientSecret != "" || in.ClearSecret,
	})
	v := s.systemView()
	if ssoErr != "" {
		v.SSOError = ssoErr
	}
	writeJSON(w, 200, v)
}

// testOIDC checks that an issuer URL serves a valid discovery document.
func (s *Server) testOIDC(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Issuer string `json:"issuer"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	c := *s.conf()
	c.OIDCIssuer = strings.TrimSpace(in.Issuer)
	c.OIDCClientID = "test"
	oc, err := discoverOIDC(r.Context(), &c)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var meta struct {
		Auth  string   `json:"authorization_endpoint"`
		Token string   `json:"token_endpoint"`
		Algs  []string `json:"id_token_signing_alg_values_supported"`
	}
	_ = oc.provider.Claims(&meta)
	writeJSON(w, 200, map[string]any{"ok": true, "authorization_endpoint": meta.Auth, "token_endpoint": meta.Token, "end_session_endpoint": oc.endSession, "algs": meta.Algs})
}

// changePassword lets a local account change its own password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	var hash *string
	if err := s.db.QueryRow(r.Context(), `SELECT password_hash FROM users WHERE id=$1`, u.ID).Scan(&hash); err != nil || hash == nil {
		if u.Local {
			fail(w, badRequest("this account's password comes from LOCAL_ADMIN_PASSWORD in .env; change it there"))
		} else {
			fail(w, badRequest("this account signs in with SSO; change the password in Authentik"))
		}
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(in.Current)) != nil {
		fail(w, badRequest("current password is wrong"))
		return
	}
	if len(in.New) < 10 {
		fail(w, badRequest("new password must be at least 10 characters"))
		return
	}
	nh, _ := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	_, _ = s.db.Exec(r.Context(), `UPDATE users SET password_hash=$2 WHERE id=$1`, u.ID, string(nh))
	s.audit(r.Context(), u, r, "user.password_changed", "user", u.ID, nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// relayValue hides the relay when it just defaults to the ID server host.
func relayValue(c *Config) string {
	if c.RustDeskRelay == c.RustDeskHost {
		return ""
	}
	return c.RustDeskRelay
}
