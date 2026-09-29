package server

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is loaded from environment variables.
type Config struct {
	Listen      string
	PublicURL   string // e.g. https://remote.hardyvpn.online
	DatabaseURL string
	WebDir      string // built dashboard
	AgentDir    string // compiled agent binaries: hardy-agent-<os>-<arch>[.exe]

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCScopes       []string
	OIDCAdminGroups  []string
	OIDCTechGroups   []string
	OIDCViewerGroups []string
	OIDCDefaultRole  string // role for users in none of the groups ("" = deny)
	OIDCLogoutURL    string // optional end-session URL override

	LocalAdminEmail    string
	LocalAdminPassword string

	RustDeskHost    string // host (or host:port) of hbbs, e.g. rustdesk.hardyvpn.online
	RustDeskRelay   string // host of hbbr (defaults to RustDeskHost)
	RustDeskKey     string
	RustDeskKeyFile string

	MetricsRetentionDays int
	SessionHours         int
	TrustProxy           bool
	CompanyName          string
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envList(k string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(k), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

// LoadConfig reads configuration from the environment.
func LoadConfig() (*Config, error) {
	c := &Config{
		Listen:             env("LISTEN", ":8080"),
		PublicURL:          strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		DatabaseURL:        env("DATABASE_URL", "postgres://hardy:hardy@localhost:5432/hardy?sslmode=disable"),
		WebDir:             env("WEB_DIR", "./web/dist"),
		AgentDir:           env("AGENT_DIR", "./dist/agents"),
		OIDCIssuer:         env("OIDC_ISSUER", ""),
		OIDCClientID:       env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:   env("OIDC_CLIENT_SECRET", ""),
		OIDCScopes:         envList("OIDC_SCOPES"),
		OIDCAdminGroups:    envList("OIDC_ADMIN_GROUPS"),
		OIDCTechGroups:     envList("OIDC_TECH_GROUPS"),
		OIDCViewerGroups:   envList("OIDC_VIEWER_GROUPS"),
		OIDCDefaultRole:    env("OIDC_DEFAULT_ROLE", ""),
		OIDCLogoutURL:      env("OIDC_LOGOUT_URL", ""),
		LocalAdminEmail:    env("LOCAL_ADMIN_EMAIL", ""),
		LocalAdminPassword: env("LOCAL_ADMIN_PASSWORD", ""),
		RustDeskHost:       env("RUSTDESK_HOST", ""),
		RustDeskRelay:      env("RUSTDESK_RELAY", ""),
		RustDeskKey:        env("RUSTDESK_KEY", ""),
		RustDeskKeyFile:    env("RUSTDESK_KEY_FILE", "/rustdesk/id_ed25519.pub"),
		MetricsRetentionDays: envInt("METRICS_RETENTION_DAYS", 14),
		SessionHours:       envInt("SESSION_HOURS", 12),
		TrustProxy:         env("TRUST_PROXY", "true") == "true",
		CompanyName:        env("COMPANY_NAME", "Hardy RMM"),
	}
	if len(c.OIDCScopes) == 0 {
		c.OIDCScopes = []string{"openid", "profile", "email"}
	}
	if len(c.OIDCAdminGroups) == 0 {
		c.OIDCAdminGroups = []string{"RMM Admins"}
	}
	if len(c.OIDCTechGroups) == 0 {
		c.OIDCTechGroups = []string{"RMM Technicians"}
	}
	if c.RustDeskRelay == "" {
		c.RustDeskRelay = c.RustDeskHost
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("PUBLIC_URL %q is not a valid URL", c.PublicURL)
	}
	if c.OIDCIssuer == "" && c.LocalAdminPassword == "" {
		return nil, fmt.Errorf("configure OIDC_ISSUER (Authentik) and/or LOCAL_ADMIN_PASSWORD so someone can sign in")
	}
	if c.OIDCDefaultRole != "" && !validRole(c.OIDCDefaultRole) {
		return nil, fmt.Errorf("OIDC_DEFAULT_ROLE must be admin, technician or viewer")
	}
	return c, nil
}

// OIDCEnabled reports whether SSO is configured.
func (c *Config) OIDCEnabled() bool { return c.OIDCIssuer != "" && c.OIDCClientID != "" }

// Secure reports whether the public URL is HTTPS (for cookie flags).
func (c *Config) Secure() bool { return strings.HasPrefix(c.PublicURL, "https://") }

// WSURL returns the websocket base for agents.
func (c *Config) WSURL() string {
	if strings.HasPrefix(c.PublicURL, "https://") {
		return "wss://" + strings.TrimPrefix(c.PublicURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(c.PublicURL, "http://")
}

// RustDeskPublicKey returns the hbbs public key if known.
func (c *Config) RustDeskPublicKey() string {
	if c.RustDeskKey != "" {
		return c.RustDeskKey
	}
	if c.RustDeskKeyFile != "" {
		if b, err := os.ReadFile(c.RustDeskKeyFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func validRole(r string) bool { return r == "admin" || r == "technician" || r == "viewer" }

func roleRank(r string) int {
	switch r {
	case "admin":
		return 3
	case "technician":
		return 2
	case "viewer":
		return 1
	}
	return 0
}
