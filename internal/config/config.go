// Package config exposes the env-backed runtime configuration for satellites-v4
// binaries. Load() reads the environment, falls back to defaults, validates
// required fields for the selected environment, and returns a typed Config.
//
// Env var precedence is flat — there is no TOML file in v4. Subsequent stories
// introduce auth + DB config under this same package.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the validated runtime configuration for a satellites binary.
type Config struct {
	// Port is the HTTP listen port. Default 8080.
	Port int

	// Env is the deployment environment. Canonical values: "dev" or "prod".
	Env string

	// LogLevel is the arbor log level ("trace", "debug", "info", "warn", "error").
	LogLevel string

	// DevMode relaxes production-only gates (required secrets, strict CORS).
	// Defaults true when Env=="dev".
	DevMode bool

	// DBDSN is the SurrealDB connection string. Required when Env=="prod"; the
	// concrete format is finalised in a later story (see feature-order:10.9).
	DBDSN string

	// FlyMachineID is injected by Fly.io at container start. Empty off-Fly.
	FlyMachineID string

	// DevUsername is the fixed credential allowed when DevMode is active
	// (Env != "prod" && DevMode == true). Empty disables DevMode login.
	DevUsername string

	// DevPassword is the fixed DevMode password. Never logged.
	DevPassword string

	// OAuth provider credentials (optional; empty disables the provider).
	GoogleClientID       string
	GoogleClientSecret   string
	GithubClientID       string
	GithubClientSecret   string
	OAuthRedirectBaseURL string

	// APIKeys are Bearer tokens accepted on /mcp when a session cookie is
	// absent. Typical use: CI agents + the local Claude harness. Loaded from
	// SATELLITES_API_KEYS (comma-separated).
	APIKeys []string

	// DocsDir is the container-side path containing the mounted docs
	// volume that document_ingest_file reads from. Defaults to /app/docs.
	DocsDir string
}

// Load reads the environment and returns a validated Config. Missing required
// fields (e.g. DB_DSN in prod) return a structured error; callers should log
// via arbor and exit non-zero.
func Load() (*Config, error) {
	cfg := &Config{
		Port:         8080,
		Env:          "dev",
		LogLevel:     "info",
		DevMode:      true,
		DBDSN:        "",
		FlyMachineID: "",
		DocsDir:      "/app/docs",
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
	if c.Env == "prod" && strings.TrimSpace(c.DBDSN) == "" {
		return fmt.Errorf("DB_DSN is required when ENV=prod")
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
