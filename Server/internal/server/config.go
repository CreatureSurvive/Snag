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
	// Seeds are projects from SNAG_API_KEYS and SNAG_PROJECTS_FILE.
	Seeds          []ProjectSeed
	AdminUser      string
	AdminPassword  string
	MaxUploadBytes int64
	// RatePerHour is the number of reports one client IP may submit per hour.
	RatePerHour   int
	TrustProxy    bool
	RetentionDays int

	// S3 stores binary attachments in an S3-compatible bucket when set.
	S3 *S3Config

	// MetricsToken lets Prometheus scrape /metrics with a Bearer token instead of the admin login.
	MetricsToken string
	// ProjectStorageMB caps each project's attachments (0 = no limit); projects can override it.
	ProjectStorageMB int

	// InboundEmailSecret enables POST /api/v1/inbound-email for reporters' email replies.
	InboundEmailSecret string

	// BackupDir, when set, receives a backup every BackupHours; the newest BackupKeep are kept.
	BackupDir   string
	BackupHours int
	BackupKeep  int

	// GitHubToken is the default token for projects that forward to GitHub without their own.
	GitHubToken  string
	GitHubAPIURL string
	// GitHubWebhookSecret enables /api/v1/github/webhook for two-way sync.
	GitHubWebhookSecret string
	SMTP                *SMTPConfig
	// NotifyPerHour caps push notifications per project per hour.
	NotifyPerHour int
}

// GitHubConfig is a resolved GitHub destination for one project.
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
	// ReplyTo is set on replies to reporters, so their answers reach you. With
	// SNAG_INBOUND_EMAIL_SECRET it gets a per-report plus-tag (support+r…@example.com).
	ReplyTo string
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
	}
	var err error
	if cfg.AdminPassword, err = secret("SNAG_ADMIN_PASSWORD"); err != nil {
		return cfg, err
	}

	// SNAG_API_KEYS=app:key,app:key2,key3 — keys without a name belong to "default".
	keys, err := secret("SNAG_API_KEYS")
	if err != nil {
		return cfg, err
	}
	envSeeds := map[string]*ProjectSeed{}
	var envOrder []string
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
		slug := Slugify(app)
		if slug == "" {
			return cfg, fmt.Errorf("SNAG_API_KEYS: %q isn't a usable project name", app)
		}
		if envSeeds[slug] == nil {
			envSeeds[slug] = &ProjectSeed{Slug: slug, Name: app}
			envOrder = append(envOrder, slug)
		}
		envSeeds[slug].Keys = append(envSeeds[slug].Keys, key)
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
	if cfg.InboundEmailSecret, err = secret("SNAG_INBOUND_EMAIL_SECRET"); err != nil {
		return cfg, err
	}
	if bucket := get("SNAG_S3_BUCKET", ""); bucket != "" {
		s3 := &S3Config{Endpoint: get("SNAG_S3_ENDPOINT", "https://s3.amazonaws.com"), Region: get("SNAG_S3_REGION", "us-east-1"),
			Bucket: bucket, AccessKey: get("SNAG_S3_ACCESS_KEY_ID", ""), Prefix: get("SNAG_S3_PREFIX", "")}
		if s3.SecretKey, err = secret("SNAG_S3_SECRET_ACCESS_KEY"); err != nil {
			return cfg, err
		}
		if s3.Prefix != "" && !strings.HasSuffix(s3.Prefix, "/") {
			s3.Prefix += "/"
		}
		cfg.S3 = s3
	}
	if cfg.MetricsToken, err = secret("SNAG_METRICS_TOKEN"); err != nil {
		return cfg, err
	}
	if cfg.ProjectStorageMB, err = intVar("SNAG_PROJECT_STORAGE_MB", 0); err != nil {
		return cfg, err
	}
	cfg.BackupDir = get("SNAG_BACKUP_DIR", "")
	if cfg.BackupHours, err = intVar("SNAG_BACKUP_HOURS", 24); err != nil {
		return cfg, err
	}
	if cfg.BackupKeep, err = intVar("SNAG_BACKUP_KEEP", 7); err != nil {
		return cfg, err
	}
	cfg.TrustProxy = get("SNAG_TRUST_PROXY", "") == "true" || get("SNAG_TRUST_PROXY", "") == "1"

	if cfg.PublicURL != "" {
		if u, err := url.Parse(cfg.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return cfg, fmt.Errorf("SNAG_PUBLIC_URL must be an http(s) URL")
		}
	}

	// Forwarding defaults for projects seeded from SNAG_API_KEYS.
	var defaults ProjectForwarding
	if cfg.GitHubToken, err = secret("SNAG_GITHUB_TOKEN"); err != nil {
		return cfg, err
	}
	cfg.GitHubAPIURL = strings.TrimRight(get("SNAG_GITHUB_API_URL", "https://api.github.com"), "/")
	if cfg.GitHubWebhookSecret, err = secret("SNAG_GITHUB_WEBHOOK_SECRET"); err != nil {
		return cfg, err
	}
	if repo := get("SNAG_GITHUB_REPO", ""); repo != "" {
		if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" {
			return cfg, fmt.Errorf("SNAG_GITHUB_REPO must be owner/repo")
		}
		defaults.GitHub = &ProjectGitHub{
			Repo:             repo,
			Labels:           splitList(get("SNAG_GITHUB_LABELS", "snag")),
			AttachmentBranch: get("SNAG_GITHUB_ATTACHMENT_BRANCH", ""),
		}
	}
	if defaults.Discord, err = secret("SNAG_DISCORD_WEBHOOK"); err != nil {
		return cfg, err
	}
	if defaults.Slack, err = secret("SNAG_SLACK_WEBHOOK"); err != nil {
		return cfg, err
	}
	if hook := get("SNAG_WEBHOOK_URL", ""); hook != "" {
		s, err := secret("SNAG_WEBHOOK_SECRET")
		if err != nil {
			return cfg, err
		}
		defaults.Webhook = &WebhookConfig{URL: hook, Secret: s}
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
			ReplyTo: get("SNAG_SMTP_REPLY_TO", ""),
			TLS:     get("SNAG_SMTP_TLS", map[bool]string{true: "tls", false: "starttls"}[port == 465]),
		}
		if smtp.From == "" {
			return cfg, fmt.Errorf("SNAG_SMTP_FROM is required when SNAG_SMTP_HOST is set")
		}
		cfg.SMTP = smtp
		defaults.Email = smtp.To
	}
	var notify *NotifyConfig
	ntfy, err := secret("SNAG_NTFY_URL")
	if err != nil {
		return cfg, err
	}
	ntfyToken, err := secret("SNAG_NTFY_TOKEN")
	if err != nil {
		return cfg, err
	}
	poUser, err := secret("SNAG_PUSHOVER_USER")
	if err != nil {
		return cfg, err
	}
	poToken, err := secret("SNAG_PUSHOVER_TOKEN")
	if err != nil {
		return cfg, err
	}
	if ntfy != "" || (poUser != "" && poToken != "") {
		notify = &NotifyConfig{Ntfy: ntfy, NtfyToken: ntfyToken, PushoverUser: poUser, PushoverToken: poToken, OnNewIssue: true, OnRegression: true}
	}
	if cfg.NotifyPerHour, err = intVar("SNAG_NOTIFY_PER_HOUR", 30); err != nil {
		return cfg, err
	}
	for _, slug := range envOrder {
		seed := envSeeds[slug]
		seed.Notify = notify
		seed.Forwarding = defaults
		cfg.Seeds = append(cfg.Seeds, *seed)
	}
	if path := get("SNAG_PROJECTS_FILE", ""); path != "" {
		seeds, err := LoadProjectsFile(path, getenv)
		if err != nil {
			return cfg, fmt.Errorf("SNAG_PROJECTS_FILE: %w", err)
		}
		cfg.Seeds = append(cfg.Seeds, seeds...)
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
