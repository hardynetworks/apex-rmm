package server

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is the effective configuration. Boot settings (listen address,
// database, paths) come only from the environment. Everything an operator
// normally changes - public URL, SSO, RustDesk - can be edited in the
// dashboard (stored in the settings table); an environment variable, when
// set, overrides the stored value and locks the field in the UI.
type Config struct {
	// boot-only (environment)
	Listen               string
	DatabaseURL          string
	WebDir               string
	AgentDir             string
	MetricsRetentionDays int
	SessionHours         int
	TrustProxy           bool
	RustDeskKeyFile      string
	AppKeyFile           string
	LocalAdminEmail      string
	LocalAdminPassword   string
	OIDCScopes           []string

	// editable in Settings (environment overrides)
	PublicURL        string
	CompanyName      string
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCAdminGroups  []string
	OIDCTechGroups   []string
	OIDCViewerGroups []string
	OIDCDefaultRole  string
	OIDCLogoutURL    string
	RustDeskHost     string
	RustDeskRelay    string
	RustDeskKey      string

	// Locked lists editable settings that are pinned by environment variables.
	Locked map[string]bool
}

// editableEnv maps setting keys (as used by the API/UI) to environment variables.
var editableEnv = map[string]string{
	"public_url":         "PUBLIC_URL",
	"company_name":       "COMPANY_NAME",
	"oidc_issuer":        "OIDC_ISSUER",
	"oidc_client_id":     "OIDC_CLIENT_ID",
	"oidc_client_secret": "OIDC_CLIENT_SECRET",
	"oidc_admin_groups":  "OIDC_ADMIN_GROUPS",
	"oidc_tech_groups":   "OIDC_TECH_GROUPS",
	"oidc_viewer_groups": "OIDC_VIEWER_GROUPS",
	"oidc_default_role":  "OIDC_DEFAULT_ROLE",
	"oidc_logout_url":    "OIDC_LOGOUT_URL",
	"rustdesk_host":      "RUSTDESK_HOST",
	"rustdesk_relay":     "RUSTDESK_RELAY",
	"rustdesk_key":       "RUSTDESK_KEY",
}

// SystemSettings is the dashboard-editable part of the configuration,
// persisted as JSON in the settings table under the key "system".
type SystemSettings struct {
	PublicURL        string   `json:"public_url"`
	CompanyName      string   `json:"company_name"`
	OIDCIssuer       string   `json:"oidc_issuer"`
	OIDCClientID     string   `json:"oidc_client_id"`
	OIDCClientSecret string   `json:"oidc_client_secret"` // encrypted at rest
	OIDCAdminGroups  []string `json:"oidc_admin_groups"`
	OIDCTechGroups   []string `json:"oidc_tech_groups"`
	OIDCViewerGroups []string `json:"oidc_viewer_groups"`
	OIDCDefaultRole  string   `json:"oidc_default_role"`
	OIDCLogoutURL    string   `json:"oidc_logout_url"`
	RustDeskHost     string   `json:"rustdesk_host"`
	RustDeskRelay    string   `json:"rustdesk_relay"`
	RustDeskKey      string   `json:"rustdesk_key"`
	SetupComplete    bool     `json:"setup_complete"`
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envList(k string) []string { return splitList(os.Getenv(k)) }

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func readSecretFile(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// databaseURL uses DATABASE_URL, or builds one from DB_* variables so the
// password can come from a Docker secret file.
func databaseURL() string {
	if v := env("DATABASE_URL", ""); v != "" {
		return v
	}
	pw := env("DB_PASSWORD", "")
	if pw == "" {
		pw = readSecretFile(env("DB_PASSWORD_FILE", ""))
	}
	if pw == "" {
		pw = "hardy"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(env("DB_USER", "hardy"), pw),
		Host:     env("DB_HOST", "localhost") + ":" + env("DB_PORT", "5432"),
		Path:     "/" + env("DB_NAME", "hardy"),
		RawQuery: "sslmode=" + env("DB_SSLMODE", "disable"),
	}
	return u.String()
}

// LoadConfig reads the boot configuration and editable defaults from the environment.
func LoadConfig() (*Config, error) {
	c := &Config{
		Listen:               env("LISTEN", ":8080"),
		DatabaseURL:          databaseURL(),
		WebDir:               env("WEB_DIR", "./web/dist"),
		AgentDir:             env("AGENT_DIR", "./dist/agents"),
		MetricsRetentionDays: envInt("METRICS_RETENTION_DAYS", 14),
		SessionHours:         envInt("SESSION_HOURS", 12),
		TrustProxy:           env("TRUST_PROXY", "true") == "true",
		RustDeskKeyFile:      env("RUSTDESK_KEY_FILE", "/rustdesk/id_ed25519.pub"),
		AppKeyFile:           env("APP_KEY_FILE", "/secrets/app_key"),
		LocalAdminEmail:      env("LOCAL_ADMIN_EMAIL", ""),
		LocalAdminPassword:   env("LOCAL_ADMIN_PASSWORD", ""),
		OIDCScopes:           envList("OIDC_SCOPES"),

		PublicURL:        strings.TrimRight(env("PUBLIC_URL", ""), "/"),
		CompanyName:      env("COMPANY_NAME", ""),
		OIDCIssuer:       env("OIDC_ISSUER", ""),
		OIDCClientID:     env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("OIDC_CLIENT_SECRET", ""),
		OIDCAdminGroups:  envList("OIDC_ADMIN_GROUPS"),
		OIDCTechGroups:   envList("OIDC_TECH_GROUPS"),
		OIDCViewerGroups: envList("OIDC_VIEWER_GROUPS"),
		OIDCDefaultRole:  env("OIDC_DEFAULT_ROLE", ""),
		OIDCLogoutURL:    env("OIDC_LOGOUT_URL", ""),
		RustDeskHost:     env("RUSTDESK_HOST", ""),
		RustDeskRelay:    env("RUSTDESK_RELAY", ""),
		RustDeskKey:      env("RUSTDESK_KEY", ""),
		Locked:           map[string]bool{},
	}
	for key, ev := range editableEnv {
		if strings.TrimSpace(os.Getenv(ev)) != "" {
			c.Locked[key] = true
		}
	}
	if len(c.OIDCScopes) == 0 {
		c.OIDCScopes = []string{"openid", "profile", "email"}
	}
	if c.PublicURL != "" {
		if err := validPublicURL(c.PublicURL); err != nil {
			return nil, err
		}
	}
	if c.OIDCDefaultRole != "" && !validRole(c.OIDCDefaultRole) {
		return nil, fmt.Errorf("OIDC_DEFAULT_ROLE must be admin, technician or viewer")
	}
	return c, nil
}

func validPublicURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("public URL %q must look like https://rmm.example.com", s)
	}
	return nil
}

// withSettings returns the effective config: stored settings fill every
// editable field not pinned by the environment, then defaults apply.
func (base *Config) withSettings(ss SystemSettings, secret string) *Config {
	c := *base
	set := func(key string, dst *string, v string) {
		if !base.Locked[key] && v != "" {
			*dst = v
		}
	}
	setList := func(key string, dst *[]string, v []string) {
		if !base.Locked[key] && len(v) > 0 {
			*dst = v
		}
	}
	set("public_url", &c.PublicURL, strings.TrimRight(ss.PublicURL, "/"))
	set("company_name", &c.CompanyName, ss.CompanyName)
	set("oidc_issuer", &c.OIDCIssuer, ss.OIDCIssuer)
	set("oidc_client_id", &c.OIDCClientID, ss.OIDCClientID)
	set("oidc_client_secret", &c.OIDCClientSecret, secret)
	setList("oidc_admin_groups", &c.OIDCAdminGroups, ss.OIDCAdminGroups)
	setList("oidc_tech_groups", &c.OIDCTechGroups, ss.OIDCTechGroups)
	setList("oidc_viewer_groups", &c.OIDCViewerGroups, ss.OIDCViewerGroups)
	set("oidc_default_role", &c.OIDCDefaultRole, ss.OIDCDefaultRole)
	set("oidc_logout_url", &c.OIDCLogoutURL, ss.OIDCLogoutURL)
	set("rustdesk_host", &c.RustDeskHost, ss.RustDeskHost)
	set("rustdesk_relay", &c.RustDeskRelay, ss.RustDeskRelay)
	set("rustdesk_key", &c.RustDeskKey, ss.RustDeskKey)
	if c.PublicURL == "" {
		c.PublicURL = "http://localhost:8080"
	}
	if c.CompanyName == "" {
		c.CompanyName = "Hardy RMM"
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
	if !validRole(c.OIDCDefaultRole) {
		c.OIDCDefaultRole = ""
	}
	return &c
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
	return readSecretFile(c.RustDeskKeyFile)
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
