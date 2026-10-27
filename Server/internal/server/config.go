package server

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is read from SNAG_* environment variables. See the README for the full list.
type Config struct {
	Addr      string
	DataDir   string
	PublicURL string
	// APIKeys maps an ingest key to the app name it belongs to.
	APIKeys        map[string]string
	AdminUser      string
	AdminPassword  string
	MaxUploadBytes int64
	// RatePerHour is the number of reports one client IP may submit per hour.
	RatePerHour   int
	TrustProxy    bool
	RetentionDays int

	GitHub  *GitHubConfig
	Discord string
	Slack   string
	Webhook *WebhookConfig
	SMTP    *SMTPConfig
}

type GitHubConfig struct {
	Token  string
	Owner  string
	Repo   string
	Labels []string
	// AttachmentBranch, when set, commits screenshots to this branch so they render in issues.
	AttachmentBranch string
	APIURL           string
}

type WebhookConfig struct {
	URL    string
	Secret string
}

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	// TLS is "starttls" (default), "tls" (implicit, port 465) or "none" (local relays only).
	TLS string
}

// LoadConfig reads the environment. getenv is os.Getenv in production.
func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	// Secrets may also come from files (Docker secrets): SNAG_X_FILE=/run/secrets/x.
	secret := func(key string) (string, error) {
		if path := strings.TrimSpace(getenv(key + "_FILE")); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("%s_FILE: %w", key, err)
			}
			return strings.TrimSpace(string(data)), nil
		}
		return strings.TrimSpace(getenv(key)), nil
	}

	cfg := Config{
		Addr:      get("SNAG_ADDR", ":8080"),
		DataDir:   get("SNAG_DATA_DIR", "/data"),
		PublicURL: strings.TrimRight(get("SNAG_PUBLIC_URL", ""), "/"),
		AdminUser: get("SNAG_ADMIN_USER", "admin"),
		APIKeys:   map[string]string{},
	}
	var err error
	if cfg.AdminPassword, err = secret("SNAG_ADMIN_PASSWORD"); err != nil {
		return cfg, err
	}

	keys, err := secret("SNAG_API_KEYS")
	if err != nil {
		return cfg, err
	}
	for _, entry := range strings.Split(keys, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		app, key := "default", entry
		if name, value, ok := strings.Cut(entry, ":"); ok {
			app, key = strings.TrimSpace(name), strings.TrimSpace(value)
		}
		if len(key) < 16 {
			return cfg, fmt.Errorf("SNAG_API_KEYS: key for %q is shorter than 16 characters", app)
		}
		cfg.APIKeys[key] = app
	}
	if len(cfg.APIKeys) == 0 {
		return cfg, fmt.Errorf("SNAG_API_KEYS is required (comma-separated, optionally app:key)")
	}

	intVar := func(key string, fallback int) (int, error) {
		v := get(key, "")
		if v == "" {
			return fallback, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s must be a non-negative integer", key)
		}
		return n, nil
	}
	mb, err := intVar("SNAG_MAX_UPLOAD_MB", 50)
	if err != nil {
		return cfg, err
	}
	cfg.MaxUploadBytes = int64(mb) << 20
	if cfg.RatePerHour, err = intVar("SNAG_RATE_LIMIT", 60); err != nil {
		return cfg, err
	}
	if cfg.RetentionDays, err = intVar("SNAG_RETENTION_DAYS", 0); err != nil {
		return cfg, err
	}
	cfg.TrustProxy = get("SNAG_TRUST_PROXY", "") == "true" || get("SNAG_TRUST_PROXY", "") == "1"

	if cfg.PublicURL != "" {
		if u, err := url.Parse(cfg.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return cfg, fmt.Errorf("SNAG_PUBLIC_URL must be an http(s) URL")
		}
	}

	// Forwarders.
	if token, err := secret("SNAG_GITHUB_TOKEN"); err != nil {
		return cfg, err
	} else if token != "" {
		owner, repo, ok := strings.Cut(get("SNAG_GITHUB_REPO", ""), "/")
		if !ok || owner == "" || repo == "" {
			return cfg, fmt.Errorf("SNAG_GITHUB_REPO must be owner/repo when SNAG_GITHUB_TOKEN is set")
		}
		cfg.GitHub = &GitHubConfig{
			Token: token, Owner: owner, Repo: repo,
			Labels:           splitList(get("SNAG_GITHUB_LABELS", "snag")),
			AttachmentBranch: get("SNAG_GITHUB_ATTACHMENT_BRANCH", ""),
			APIURL:           strings.TrimRight(get("SNAG_GITHUB_API_URL", "https://api.github.com"), "/"),
		}
	}
	if cfg.Discord, err = secret("SNAG_DISCORD_WEBHOOK"); err != nil {
		return cfg, err
	}
	if cfg.Slack, err = secret("SNAG_SLACK_WEBHOOK"); err != nil {
		return cfg, err
	}
	if hook := get("SNAG_WEBHOOK_URL", ""); hook != "" {
		s, err := secret("SNAG_WEBHOOK_SECRET")
		if err != nil {
			return cfg, err
		}
		cfg.Webhook = &WebhookConfig{URL: hook, Secret: s}
	}
	if host := get("SNAG_SMTP_HOST", ""); host != "" {
		port, err := intVar("SNAG_SMTP_PORT", 587)
		if err != nil {
			return cfg, err
		}
		password, err := secret("SNAG_SMTP_PASSWORD")
		if err != nil {
			return cfg, err
		}
		smtp := &SMTPConfig{
			Host: host, Port: port,
			Username: get("SNAG_SMTP_USERNAME", ""), Password: password,
			From: get("SNAG_SMTP_FROM", ""), To: splitList(get("SNAG_SMTP_TO", "")),
			TLS: get("SNAG_SMTP_TLS", map[bool]string{true: "tls", false: "starttls"}[port == 465]),
		}
		if smtp.From == "" || len(smtp.To) == 0 {
			return cfg, fmt.Errorf("SNAG_SMTP_FROM and SNAG_SMTP_TO are required when SNAG_SMTP_HOST is set")
		}
		cfg.SMTP = smtp
	}
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Retention returns how long reports are kept, or 0 for forever.
func (c Config) Retention() time.Duration {
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}
