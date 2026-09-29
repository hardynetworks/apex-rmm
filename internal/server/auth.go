package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "hardy_session"
	oidcCookie    = "hardy_oidc"
)

// User is the authenticated dashboard user.
type User struct {
	ID       string   `json:"id"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Username string   `json:"username"`
	Role     string   `json:"role"`
	Groups   []string `json:"groups"`
	Local    bool     `json:"local"`
}

func (u *User) Display() string {
	if u.Name != "" {
		return u.Name
	}
	if u.Email != "" {
		return u.Email
	}
	return u.Username
}

type ctxKey int

const userKey ctxKey = 1

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

// Auth handles OIDC (Authentik) and the optional local break-glass account.
type Auth struct {
	s          *Server
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	oauth      *oauth2.Config
	endSession string
	localHash  []byte
}

func newAuth(ctx context.Context, s *Server) (*Auth, error) {
	a := &Auth{s: s}
	c := s.cfg
	if c.LocalAdminPassword != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(c.LocalAdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		a.localHash = h
		if c.LocalAdminEmail == "" {
			c.LocalAdminEmail = "admin@localhost"
		}
	}
	if !c.OIDCEnabled() {
		slog.Warn("OIDC is not configured; only the local admin account can sign in")
		return a, nil
	}
	var err error
	for i := 0; i < 10; i++ {
		a.provider, err = oidc.NewProvider(ctx, c.OIDCIssuer)
		if err == nil {
			break
		}
		slog.Warn("OIDC discovery failed, retrying", "issuer", c.OIDCIssuer, "err", err)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s: %w", c.OIDCIssuer, err)
	}
	a.verifier = a.provider.Verifier(&oidc.Config{ClientID: c.OIDCClientID})
	a.oauth = &oauth2.Config{
		ClientID:     c.OIDCClientID,
		ClientSecret: c.OIDCClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  c.PublicURL + "/auth/callback",
		Scopes:       c.OIDCScopes,
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = a.provider.Claims(&extra)
	a.endSession = extra.EndSession
	if c.OIDCLogoutURL != "" {
		a.endSession = c.OIDCLogoutURL
	}
	return a, nil
}

type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.Redirect(w, r, "/login?error=sso_disabled", http.StatusFound)
		return
	}
	st := oidcState{State: randToken(24), Nonce: randToken(24), Verifier: oauth2.GenerateVerifier(), Return: safeReturn(r.URL.Query().Get("return"))}
	b, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: base64.RawURLEncoding.EncodeToString(b), Path: "/auth", HttpOnly: true, Secure: a.s.cfg.Secure(), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, a.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func safeReturn(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, "\\") {
		return "/"
	}
	return p
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.oauth == nil {
		http.Error(w, "SSO not configured", http.StatusNotFound)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		a.authError(w, "Sign-in was cancelled or failed: "+e+" "+r.URL.Query().Get("error_description"))
		return
	}
	ck, err := r.Cookie(oidcCookie)
	if err != nil {
		a.authError(w, "Your sign-in attempt expired. Please try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: "", Path: "/auth", MaxAge: -1})
	raw, _ := base64.RawURLEncoding.DecodeString(ck.Value)
	var st oidcState
	if json.Unmarshal(raw, &st) != nil || st.State == "" || !constEq(st.State, r.URL.Query().Get("state")) {
		a.authError(w, "Invalid sign-in state. Please try again.")
		return
	}
	tok, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		slog.Warn("oidc exchange failed", "err", err)
		a.authError(w, "Could not complete sign-in with the identity provider.")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(ctx, rawID)
	if err != nil || idt.Nonce != st.Nonce {
		slog.Warn("oidc id_token invalid", "err", err)
		a.authError(w, "The identity provider returned an invalid token.")
		return
	}
	var claims struct {
		Sub      string   `json:"sub"`
		Email    string   `json:"email"`
		Name     string   `json:"name"`
		Username string   `json:"preferred_username"`
		Groups   []string `json:"groups"`
	}
	if err := idt.Claims(&claims); err != nil {
		a.authError(w, "Could not read identity claims.")
		return
	}
	if claims.Groups == nil {
		// Some providers only put groups in userinfo.
		if ui, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			var uc struct {
				Groups []string `json:"groups"`
			}
			if ui.Claims(&uc) == nil {
				claims.Groups = uc.Groups
			}
		}
	}
	role := a.mapRole(claims.Groups)
	if role == "" {
		slog.Warn("sso login denied: no RMM group", "user", claims.Email, "groups", claims.Groups)
		a.s.audit(ctx, nil, r, "auth.denied", "user", claims.Sub, map[string]any{"email": claims.Email, "groups": claims.Groups})
		a.authError(w, "Your account ("+claims.Email+") is not in an RMM group. Ask an administrator to add you to one of: "+strings.Join(append(append([]string{}, a.s.cfg.OIDCAdminGroups...), a.s.cfg.OIDCTechGroups...), ", "))
		return
	}
	var uid string
	err = a.s.db.QueryRow(ctx, `
		INSERT INTO users (subject, email, name, username, role, groups, last_login)
		VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (subject) DO UPDATE SET email=EXCLUDED.email, name=EXCLUDED.name, username=EXCLUDED.username,
			role=EXCLUDED.role, groups=EXCLUDED.groups, last_login=now()
		RETURNING id`, claims.Sub, claims.Email, claims.Name, claims.Username, role, nz(claims.Groups)).Scan(&uid)
	if err != nil {
		fail(w, err)
		return
	}
	var disabled bool
	_ = a.s.db.QueryRow(ctx, `SELECT disabled FROM users WHERE id=$1`, uid).Scan(&disabled)
	if disabled {
		a.authError(w, "Your RMM account has been disabled.")
		return
	}
	if err := a.startSession(w, r, uid, rawID); err != nil {
		fail(w, err)
		return
	}
	u := &User{ID: uid, Email: claims.Email, Name: claims.Name, Role: role}
	a.s.audit(ctx, u, r, "auth.login", "user", uid, map[string]any{"method": "sso"})
	http.Redirect(w, r, st.Return, http.StatusFound)
}

func (a *Auth) mapRole(groups []string) string {
	c := a.s.cfg
	for _, g := range groups {
		if contains(c.OIDCAdminGroups, g) {
			return "admin"
		}
	}
	for _, g := range groups {
		if contains(c.OIDCTechGroups, g) {
			return "technician"
		}
	}
	for _, g := range groups {
		if contains(c.OIDCViewerGroups, g) {
			return "viewer"
		}
	}
	return c.OIDCDefaultRole
}

func (a *Auth) authError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta name=viewport content="width=device-width"><title>Sign-in problem</title>
<body style="font-family:system-ui;background:#0f172a;color:#e2e8f0;display:grid;place-items:center;min-height:100vh;margin:0">
<div style="max-width:440px;padding:32px;background:#1e293b;border-radius:12px"><h2 style="margin-top:0">Sign-in problem</h2><p>%s</p>
<a href="/login" style="color:#60a5fa">Back to sign in</a></div>`, html.EscapeString(msg))
}

func (a *Auth) startSession(w http.ResponseWriter, r *http.Request, uid, idToken string) error {
	sid := randToken(32)
	exp := time.Now().Add(time.Duration(a.s.cfg.SessionHours) * time.Hour)
	_, err := a.s.db.Exec(r.Context(), `INSERT INTO sessions (id, user_id, id_token, ip, user_agent, expires_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		sha256Hex(sid), uid, idToken, a.s.clientIP(r), r.UserAgent(), exp)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sid, Path: "/", HttpOnly: true, Secure: a.s.cfg.Secure(), SameSite: http.SameSiteLaxMode, Expires: exp})
	return nil
}

func (a *Auth) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	time.Sleep(300 * time.Millisecond) // slow down guessing
	if a.localHash == nil || !strings.EqualFold(in.Email, a.s.cfg.LocalAdminEmail) || bcrypt.CompareHashAndPassword(a.localHash, []byte(in.Password)) != nil {
		a.s.audit(r.Context(), nil, r, "auth.local_failed", "user", "", map[string]any{"email": in.Email})
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	ctx := r.Context()
	var uid string
	err := a.s.db.QueryRow(ctx, `SELECT id FROM users WHERE subject IS NULL AND lower(email)=lower($1)`, in.Email).Scan(&uid)
	if err != nil {
		err = a.s.db.QueryRow(ctx, `INSERT INTO users (email, name, username, role) VALUES ($1,'Local Admin','admin','admin') RETURNING id`, in.Email).Scan(&uid)
		if err != nil {
			fail(w, err)
			return
		}
	}
	_, _ = a.s.db.Exec(ctx, `UPDATE users SET last_login=now(), role='admin' WHERE id=$1`, uid)
	if err := a.startSession(w, r, uid, ""); err != nil {
		fail(w, err)
		return
	}
	a.s.audit(ctx, &User{ID: uid, Email: in.Email, Name: "Local Admin"}, r, "auth.login", "user", uid, map[string]any{"method": "local"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"redirect": "/login"}
	if ck, err := r.Cookie(sessionCookie); err == nil {
		var idToken string
		_ = a.s.db.QueryRow(r.Context(), `DELETE FROM sessions WHERE id=$1 RETURNING id_token`, sha256Hex(ck.Value)).Scan(&idToken)
		if idToken != "" && a.endSession != "" {
			q := url.Values{"id_token_hint": {idToken}, "post_logout_redirect_uri": {a.s.cfg.PublicURL + "/login"}}
			out["redirect"] = a.endSession + "?" + q.Encode()
		}
	}
	if u := userFrom(r); u != nil {
		a.s.audit(r.Context(), u, r, "auth.logout", "user", u.ID, nil)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, out)
}

func (a *Auth) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"sso":     a.oauth != nil,
		"local":   a.localHash != nil,
		"company": a.s.cfg.CompanyName,
	})
}

// lookupSession resolves the session cookie to a user.
func (a *Auth) lookupSession(r *http.Request) (*User, error) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return nil, errors.New("no session")
	}
	u := &User{}
	var subject *string
	err = a.s.db.QueryRow(r.Context(), `
		SELECT u.id, u.email, u.name, u.username, u.role, u.groups, u.subject
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.id=$1 AND s.expires_at > now() AND NOT u.disabled`, sha256Hex(ck.Value)).
		Scan(&u.ID, &u.Email, &u.Name, &u.Username, &u.Role, &u.Groups, &subject)
	if err != nil {
		return nil, err
	}
	u.Local = subject == nil
	return u, nil
}

// middleware: attaches the user; rejects unauthenticated API requests and enforces CSRF header.
func (a *Auth) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.lookupSession(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Hardy-CSRF") != "1" {
			writeErr(w, http.StatusForbidden, "missing CSRF header")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// requireRole returns middleware ensuring the user has at least the role.
func requireRole(min string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := userFrom(r)
			if u == nil || roleRank(u.Role) < roleRank(min) {
				writeErr(w, http.StatusForbidden, "requires "+min+" role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// checkOrigin validates browser WebSocket origins against PUBLIC_URL.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	pu, _ := url.Parse(s.cfg.PublicURL)
	return strings.EqualFold(u.Host, pu.Host) || strings.EqualFold(u.Host, r.Host)
}

func hashShort(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:8])
}
