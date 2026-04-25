// Package config exposes the env-backed runtime configuration for satellites-v4
// binaries. Load() reads the environment, falls back to defaults, validates
// required fields for the selected environment, and returns a typed Config.
//
// Env var precedence is flat — there is no TOML file in v4. Every exported
// field on Config has a default assigned in Load() (so the binary boots in dev
// with zero env vars set) or an explicit prod-required validation in
// validate(). The single source of truth for the env-var → field mapping is
// the slice returned by Describe().
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the validated runtime configuration for a satellites binary.
//
// Every exported field maps to exactly one env var and has either a sensible
// default (set in Load) or a prod-required validation (in validate). Run
// Describe() for the canonical (field, env, default, prod_required) table.
type Config struct {
	// Port is the HTTP listen port. Default 8080. Env: PORT or SATELLITES_PORT.
	Port int

	// Env is the deployment environment. Canonical values: "dev" or "prod".
	// Env: ENV. Default "dev".
	Env string

	// LogLevel is the arbor log level ("trace", "debug", "info", "warn",
	// "error"). Env: LOG_LEVEL. Default "info".
	LogLevel string

	// DevMode relaxes production-only gates (required secrets, strict CORS)
	// and turns on the dev-mode quick-signin and DEV portal affordances.
	// Defaults true when Env=="dev". Env: DEV_MODE.
	DevMode bool

	// DBDSN is the SurrealDB connection string. Required when Env=="prod"; in
	// dev an empty value boots the server without DB-backed verbs.
	// Env: DB_DSN.
	DBDSN string

	// FlyMachineID is injected by Fly.io at container start. Empty off-Fly.
	// Env: FLY_MACHINE_ID.
	FlyMachineID string

	// DevUsername is the fixed credential allowed when DevMode is active. In
	// dev defaults to "dev@satellites.local" so the dev-mode quick-signin
	// works without any env vars. Empty in prod. Env: DEV_USERNAME.
	DevUsername string

	// DevPassword is the fixed DevMode password. In dev defaults to "dev123".
	// Never logged. Empty in prod. Env: DEV_PASSWORD.
	DevPassword string

	// GoogleClientID is the OAuth 2.0 client id. Env: GOOGLE_CLIENT_ID. Empty
	// disables the provider. Must be paired with GoogleClientSecret — half a
	// credential is rejected by validate().
	GoogleClientID string

	// GoogleClientSecret is the OAuth 2.0 client secret. Never logged.
	// Env: GOOGLE_CLIENT_SECRET.
	GoogleClientSecret string

	// GithubClientID is the OAuth 2.0 client id for the GitHub provider.
	// Env: GITHUB_CLIENT_ID. Empty disables the provider; must be paired with
	// GithubClientSecret.
	GithubClientID string

	// GithubClientSecret is the GitHub OAuth client secret. Never logged.
	// Env: GITHUB_CLIENT_SECRET.
	GithubClientSecret string

	// OAuthRedirectBaseURL is the absolute base URL the auth handlers append
	// the per-provider callback path to. In dev defaults to
	// "http://localhost:<Port>" so OAuth works on `go run` with zero env
	// vars. Required (non-empty) in prod when any provider is configured.
	// Env: OAUTH_REDIRECT_BASE_URL.
	OAuthRedirectBaseURL string

	// OAuthTokenCacheTTL is how long the MCP-side OAuth token validator
	// caches a successful provider lookup. Default 5m. Range 1s..1h. Env:
	// OAUTH_TOKEN_CACHE_TTL (Go duration: "5m", "30s", "1h").
	OAuthTokenCacheTTL time.Duration

	// APIKeys are Bearer tokens accepted on /mcp when a session cookie is
	// absent. Typical use: CI agents + the local Claude harness. Loaded from
	// SATELLITES_API_KEYS (comma-separated). Empty disables Bearer-API-key
	// auth.
	APIKeys []string

	// DocsDir is the container-side path containing the mounted docs
	// volume that document_ingest_file reads from. Defaults to /app/docs.
	// Env: DOCS_DIR.
	DocsDir string

	// GrantsEnforced toggles the mcpserver grant middleware's enforcement
	// mode. When false (the current default), the middleware is a
	// pass-through. When true, MCP verbs outside the bootstrap allowlist
	// are rejected unless the caller holds a role-grant whose effective
	// verb allowlist covers the tool. Env: SATELLITES_GRANTS_ENFORCED.
	GrantsEnforced bool
}

// FieldDoc is one entry in the Describe() table — the single source of truth
// for the (config field, env var, default, prod-required) mapping.
type FieldDoc struct {
	// Field is the exported Go field name on Config.
	Field string
	// Env is the env var name read by Load().
	Env string
	// Default is a human-readable rendering of the dev-mode default.
	Default string
	// ProdRequired is true when validate() rejects the empty value in prod.
	ProdRequired bool
	// Description is a one-line summary suitable for an --env-help dump.
	Description string
}

// describeTable is the canonical Config field documentation. Adding a new
// exported field to Config without adding an entry here trips the
// reflection-based doc-coverage test in config_test.go.
var describeTable = []FieldDoc{
	{Field: "Port", Env: "PORT", Default: "8080", Description: "HTTP listen port (1..65535). SATELLITES_PORT also accepted."},
	{Field: "Env", Env: "ENV", Default: "dev", Description: "Deployment environment: dev or prod."},
	{Field: "LogLevel", Env: "LOG_LEVEL", Default: "info", Description: "Arbor log level: trace, debug, info, warn, error."},
	{Field: "DevMode", Env: "DEV_MODE", Default: "true (when ENV=dev) / false (when ENV=prod)", Description: "Enables dev-mode quick-signin and DEV portal affordances."},
	{Field: "DBDSN", Env: "DB_DSN", Default: "(empty in dev — DB-backed verbs disabled)", ProdRequired: true, Description: "SurrealDB connection string. Required in prod."},
	{Field: "FlyMachineID", Env: "FLY_MACHINE_ID", Default: "(injected by Fly.io at container start; empty off-Fly)", Description: "Fly.io machine identifier; passes through to /healthz and logs."},
	{Field: "DevUsername", Env: "DEV_USERNAME", Default: "dev@satellites.local (dev) / (empty) (prod)", Description: "Fixed credential username for DevMode signin."},
	{Field: "DevPassword", Env: "DEV_PASSWORD", Default: "dev123 (dev) / (empty) (prod)", Description: "Fixed credential password for DevMode signin. Never logged."},
	{Field: "GoogleClientID", Env: "GOOGLE_CLIENT_ID", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client id for Google. Pair with GOOGLE_CLIENT_SECRET."},
	{Field: "GoogleClientSecret", Env: "GOOGLE_CLIENT_SECRET", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client secret for Google. Never logged."},
	{Field: "GithubClientID", Env: "GITHUB_CLIENT_ID", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client id for GitHub. Pair with GITHUB_CLIENT_SECRET."},
	{Field: "GithubClientSecret", Env: "GITHUB_CLIENT_SECRET", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client secret for GitHub. Never logged."},
	{Field: "OAuthRedirectBaseURL", Env: "OAUTH_REDIRECT_BASE_URL", Default: "http://localhost:<Port> (dev) / (empty) (prod)", Description: "Base URL for OAuth callback redirects. Required in prod when any provider is configured."},
	{Field: "OAuthTokenCacheTTL", Env: "OAUTH_TOKEN_CACHE_TTL", Default: "5m", Description: "How long the MCP-side OAuth token validator caches a successful provider lookup."},
	{Field: "APIKeys", Env: "SATELLITES_API_KEYS", Default: "(empty — Bearer-API-key auth disabled)", Description: "Comma-separated Bearer tokens accepted on /mcp when a session cookie is absent."},
	{Field: "DocsDir", Env: "DOCS_DIR", Default: "/app/docs", Description: "Container-side docs volume path read by document_ingest_file."},
	{Field: "GrantsEnforced", Env: "SATELLITES_GRANTS_ENFORCED", Default: "false", Description: "When true, MCP verbs outside the bootstrap allowlist require a covering role-grant."},
}

// Describe returns the canonical (Field, Env, Default, ProdRequired,
// Description) table for the Config type. Use this in admin / --env-help
// surfaces; do not duplicate the mapping elsewhere.
func Describe() []FieldDoc {
	out := make([]FieldDoc, len(describeTable))
	copy(out, describeTable)
	return out
}

// validLogLevels mirrors the level strings accepted by internal/arbor.New.
var validLogLevels = map[string]struct{}{
	"trace": {}, "debug": {}, "info": {}, "warn": {}, "error": {},
}

// Load reads the environment and returns a validated Config. Missing required
// fields (e.g. DB_DSN in prod) return a structured error; callers should log
// via arbor and exit non-zero.
func Load() (*Config, error) {
	cfg := &Config{
		Port:               8080,
		Env:                "dev",
		LogLevel:           "info",
		DevMode:            true,
		DBDSN:              "",
		FlyMachineID:       "",
		DocsDir:            "/app/docs",
		OAuthTokenCacheTTL: 5 * time.Minute,
	}

	if v := firstNonEmpty(os.Getenv("PORT"), os.Getenv("SATELLITES_PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid PORT %q: %w", v, err)
		}
		cfg.Port = p
	}
	if v := os.Getenv("ENV"); v != "" {
		cfg.Env = normaliseEnv(v)
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if v := os.Getenv("DEV_MODE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid DEV_MODE %q: %w", v, err)
		}
		cfg.DevMode = b
	} else {
		cfg.DevMode = cfg.Env == "dev"
	}

	// Dev-mode-only quick-signin defaults: only apply when DevMode is
	// active AND the env vars are absent. Production sets neither default
	// to keep secrets out of binaries.
	if cfg.DevMode {
		cfg.DevUsername = "dev@satellites.local"
		cfg.DevPassword = "dev123"
		cfg.OAuthRedirectBaseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
	}

	if v := os.Getenv("DB_DSN"); v != "" {
		cfg.DBDSN = v
	}
	if v := os.Getenv("FLY_MACHINE_ID"); v != "" {
		cfg.FlyMachineID = v
	}
	if v := os.Getenv("DEV_USERNAME"); v != "" {
		cfg.DevUsername = v
	}
	if v := os.Getenv("DEV_PASSWORD"); v != "" {
		cfg.DevPassword = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_ID"); v != "" {
		cfg.GoogleClientID = v
	}
	if v := os.Getenv("GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.GoogleClientSecret = v
	}
	if v := os.Getenv("GITHUB_CLIENT_ID"); v != "" {
		cfg.GithubClientID = v
	}
	if v := os.Getenv("GITHUB_CLIENT_SECRET"); v != "" {
		cfg.GithubClientSecret = v
	}
	if v := os.Getenv("OAUTH_REDIRECT_BASE_URL"); v != "" {
		cfg.OAuthRedirectBaseURL = v
	}
	if v := os.Getenv("OAUTH_TOKEN_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid OAUTH_TOKEN_CACHE_TTL %q: %w", v, err)
		}
		cfg.OAuthTokenCacheTTL = d
	}
	if v := os.Getenv("SATELLITES_API_KEYS"); v != "" {
		for _, part := range strings.Split(v, ",") {
			if k := strings.TrimSpace(part); k != "" {
				cfg.APIKeys = append(cfg.APIKeys, k)
			}
		}
	}
	if v := os.Getenv("DOCS_DIR"); v != "" {
		cfg.DocsDir = v
	}
	if v := os.Getenv("SATELLITES_GRANTS_ENFORCED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid SATELLITES_GRANTS_ENFORCED %q: %w", v, err)
		}
		cfg.GrantsEnforced = b
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port out of range: %d (must be 1..65535)", c.Port)
	}
	if c.Env != "dev" && c.Env != "prod" {
		return fmt.Errorf("invalid ENV %q (must be dev or prod)", c.Env)
	}
	if _, ok := validLogLevels[c.LogLevel]; !ok {
		return fmt.Errorf("invalid LOG_LEVEL %q (must be trace, debug, info, warn, or error)", c.LogLevel)
	}
	if c.OAuthTokenCacheTTL < time.Second || c.OAuthTokenCacheTTL > time.Hour {
		return fmt.Errorf("OAUTH_TOKEN_CACHE_TTL out of range: %s (must be 1s..1h)", c.OAuthTokenCacheTTL)
	}
	if c.Env == "prod" && strings.TrimSpace(c.DBDSN) == "" {
		return fmt.Errorf("DB_DSN is required when ENV=prod")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return fmt.Errorf("GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET must be set together (got id=%t, secret=%t)", c.GoogleClientID != "", c.GoogleClientSecret != "")
	}
	if (c.GithubClientID == "") != (c.GithubClientSecret == "") {
		return fmt.Errorf("GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET must be set together (got id=%t, secret=%t)", c.GithubClientID != "", c.GithubClientSecret != "")
	}
	hasOAuth := c.GoogleClientID != "" || c.GithubClientID != ""
	if c.Env == "prod" && hasOAuth && strings.TrimSpace(c.OAuthRedirectBaseURL) == "" {
		return fmt.Errorf("OAUTH_REDIRECT_BASE_URL is required when ENV=prod and any OAuth provider is configured")
	}
	return nil
}

func normaliseEnv(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "dev", "development":
		return "dev"
	case "prod", "production":
		return "prod"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
