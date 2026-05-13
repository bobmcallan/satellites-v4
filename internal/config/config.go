// Package config exposes the runtime configuration for satellites-v4
// binaries. Load() resolves config from three layered sources — code
// defaults, an optional TOML file, and process env vars — and returns
// the resulting Config alongside a slice of operator-facing warnings.
//
// Resolution order (highest to lowest precedence):
//
//  1. Process env var (all prefixed with SATELLITES_).
//  2. TOML file (path resolved via SATELLITES_CONFIG, else ./satellites.toml).
//  3. Code default (set in defaults()).
//
// The service ALWAYS boots. Malformed env values, out-of-range numbers,
// missing-but-required prod secrets — every problem degrades to a
// warning string and the affected field falls back to its code default.
// Callers iterate the returned warnings and log them via arbor; nothing
// in this package logs directly, so config stays free of an arbor
// import cycle.
//
// Every exported field on Config has a default assigned in defaults() so
// the binary boots in prod with zero env vars and no TOML file. The
// single source of truth for the (field, env override, default,
// prod-recommended) mapping is the slice returned by Describe().
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the runtime configuration for a satellites binary.
//
// Every exported field maps to one TOML key (via the `toml` struct tag)
// and one SATELLITES_-prefixed env var override. Run Describe() for the
// canonical (field, env, default, prod_recommended) table.
type Config struct {
	// Port is the HTTP listen port. Default 8080. Out-of-range values
	// fall back to the default with a warning.
	// Env override: SATELLITES_PORT.
	Port int `toml:"port"`

	// Env is the deployment environment. Canonical values: "dev" or "prod".
	// Default "prod". Env override: SATELLITES_ENV. Unknown values fall
	// back to the default with a warning.
	Env string `toml:"env"`

	// LogLevel is the arbor log level ("trace", "debug", "info", "warn",
	// "error"). Default "info". Env override: SATELLITES_LOG_LEVEL.
	LogLevel string `toml:"log_level"`

	// DevMode relaxes production-only gates (required secrets, strict CORS)
	// and turns on the dev-mode quick-signin and DEV portal affordances.
	// Default false. Env override: SATELLITES_DEV_MODE.
	DevMode bool `toml:"dev_mode"`

	// DBDSN is the SurrealDB connection string. Recommended in prod; the
	// service boots with DB-backed verbs disabled when empty (a warning is
	// logged in prod). Env override: SATELLITES_DB_DSN.
	DBDSN string `toml:"db_dsn"`

	// DevUsername is the fixed credential allowed when DevMode is active.
	// In dev defaults to "dev@satellites.local" so the dev-mode quick-signin
	// works without any env vars. Empty in prod.
	// Env override: SATELLITES_DEV_USERNAME.
	DevUsername string `toml:"dev_username"`

	// DevPassword is the fixed DevMode password. In dev defaults to "dev123".
	// Never logged. Empty in prod. Env override: SATELLITES_DEV_PASSWORD.
	DevPassword string `toml:"dev_password"`

	// GoogleClientID is the OAuth 2.0 client id. Empty disables the provider.
	// Env override: SATELLITES_GOOGLE_CLIENT_ID. A half-set credential
	// (id without secret, or vice versa) disables the provider with a
	// warning rather than failing boot.
	GoogleClientID string `toml:"google_client_id"`

	// GoogleClientSecret is the OAuth 2.0 client secret. Never logged.
	// Env override: SATELLITES_GOOGLE_CLIENT_SECRET.
	GoogleClientSecret string `toml:"google_client_secret"`

	// GithubClientID is the OAuth 2.0 client id for the GitHub provider.
	// Empty disables the provider; half-set degrades to disabled with a
	// warning. Env override: SATELLITES_GITHUB_CLIENT_ID.
	GithubClientID string `toml:"github_client_id"`

	// GithubClientSecret is the GitHub OAuth client secret. Never logged.
	// Env override: SATELLITES_GITHUB_CLIENT_SECRET.
	GithubClientSecret string `toml:"github_client_secret"`

	// OAuthRedirectBaseURL is the absolute base URL the auth handlers append
	// the per-provider callback path to. In dev defaults to
	// "http://localhost:<Port>" so OAuth works on `go run` with zero env
	// vars. Recommended in prod when any provider is configured; empty in
	// prod logs a warning if OAuth is enabled.
	// Env override: SATELLITES_OAUTH_REDIRECT_BASE_URL.
	OAuthRedirectBaseURL string `toml:"oauth_redirect_base_url"`

	// PublicURL is the externally-reachable base URL of the satellites
	// server. Used to derive each project's MCP connection string
	// (`<PublicURL>/mcp?project_id=<id>`) when project.MCPURL is unset.
	// Empty is allowed; the project meta panel renders an explicit
	// "not configured" empty-state and the derived mcp_url field on
	// project_get returns empty.
	// Env override: SATELLITES_PUBLIC_URL.
	PublicURL string `toml:"public_url"`

	// OAuthTokenCacheTTL is how long the MCP-side OAuth token validator
	// caches a successful provider lookup. Default 4h. Out-of-range or
	// unparseable values fall back to the default with a warning.
	// Env override: SATELLITES_OAUTH_TOKEN_CACHE_TTL (Go duration:
	// "5m", "30s", "1h").
	OAuthTokenCacheTTL time.Duration `toml:"oauth_token_cache_ttl"`

	// JWTSecret signs satellites-issued OAuth access tokens (RFC 9728 /
	// 8414 / 7591 chain on /oauth/*). Empty triggers a startup warning
	// and a per-boot random key — every restart invalidates outstanding
	// MCP access tokens. Set a stable value in prod.
	// Env override: SATELLITES_JWT_SECRET.
	JWTSecret string `toml:"jwt_secret"`

	// OAuthIssuer is the absolute base URL the OAuth-AS announces in its
	// discovery metadata and JWT iss claim. Empty derives the issuer
	// from each request's host + X-Forwarded-Proto, which is correct for
	// most deployments behind Fly's edge. Set when fronted by an opaque
	// proxy. Env override: SATELLITES_OAUTH_ISSUER.
	OAuthIssuer string `toml:"oauth_issuer"`

	// OAuthAccessTokenTTL bounds the lifetime of MCP access JWTs minted
	// at /oauth/token. Default 1h. Range 1m..24h.
	// Env override: SATELLITES_OAUTH_ACCESS_TOKEN_TTL.
	OAuthAccessTokenTTL time.Duration `toml:"oauth_access_token_ttl"`

	// OAuthRefreshTokenTTL bounds the lifetime of refresh tokens issued
	// alongside access tokens. Default 168h (7d). Range 1h..30d.
	// Env override: SATELLITES_OAUTH_REFRESH_TOKEN_TTL.
	OAuthRefreshTokenTTL time.Duration `toml:"oauth_refresh_token_ttl"`

	// OAuthCodeTTL bounds the lifetime of single-use authorization codes
	// minted at /oauth/authorize. Default 10m. Spec recommends ≤10m.
	// Env override: SATELLITES_OAUTH_CODE_TTL.
	OAuthCodeTTL time.Duration `toml:"oauth_code_ttl"`

	// APIKeys are Bearer tokens accepted on /mcp when a session cookie is
	// absent. Typical use: CI agents + the local Claude harness. In TOML use
	// a native array (api_keys = ["k1","k2"]); in env use the comma-separated
	// form. Empty disables Bearer-API-key auth (a warning is logged in prod).
	// Env override: SATELLITES_API_KEYS (comma-separated).
	APIKeys []string `toml:"api_keys"`

	// DocsDir is the container-side path containing the mounted docs
	// volume that document_ingest_file reads from. Defaults to /app/docs.
	// Env override: SATELLITES_DOCS_DIR.
	DocsDir string `toml:"docs_dir"`

	// GrantsEnforced toggles the mcpserver grant middleware's enforcement
	// mode. When false (the current default), the middleware is a
	// pass-through. When true, MCP verbs outside the bootstrap allowlist
	// are rejected unless the caller holds a role-grant whose effective
	// verb allowlist covers the tool.
	// Env override: SATELLITES_GRANTS_ENFORCED.
	GrantsEnforced bool `toml:"grants_enforced"`

	// GeminiAPIKey is the Google AI Studio key used by the close-time
	// reviewer (cmd/satellites/main.go::buildReviewer). Empty disables
	// the Gemini reviewer and falls back to AcceptAll. Never logged.
	// Env override: SATELLITES_GEMINI_API_KEY.
	GeminiAPIKey string `toml:"gemini_api_key"`

	// GeminiReviewModel names the model the Gemini reviewer calls. Empty
	// resolves to internal/reviewer.DefaultGeminiReviewModel
	// ("gemini-2.5-flash"). Env override: SATELLITES_GEMINI_REVIEW_MODEL.
	GeminiReviewModel string `toml:"gemini_review_model"`

	// (reviewer_service mode is resolved at boot from the system-tier KV
	// row `reviewer.service.mode` — see cmd/satellites/main.go's
	// resolveReviewerServiceMode. Application behaviour belongs in the
	// substrate's KV layer, not in infrastructure secrets or process env.)

	// EmbeddingsProvider selects the embeddings backend. Canonical values:
	// "gemini", "stub", "none" (default). Empty resolves to "none" inside
	// embeddings.New, which disables semantic search.
	// Env override: SATELLITES_EMBEDDINGS_PROVIDER.
	EmbeddingsProvider string `toml:"embeddings_provider"`

	// EmbeddingsModel is the provider-specific model id (e.g.
	// "text-embedding-004" for gemini). Empty leaves the provider's
	// default. Env override: SATELLITES_EMBEDDINGS_MODEL.
	EmbeddingsModel string `toml:"embeddings_model"`

	// EmbeddingsAPIKey is the provider's API key. Recommended when
	// EmbeddingsProvider == "gemini"; empty under "stub"/"none". Never
	// logged. Env override: SATELLITES_EMBEDDINGS_API_KEY.
	EmbeddingsAPIKey string `toml:"embeddings_api_key"`

	// EmbeddingsBaseURL overrides the embeddings provider's HTTP base
	// URL. Empty uses the provider's canonical endpoint. Tests use this
	// to point the gemini embedder at an httptest server.
	// Env override: SATELLITES_EMBEDDINGS_BASE_URL.
	EmbeddingsBaseURL string `toml:"embeddings_base_url"`

	// EmbeddingsDimension is an optional override for the vector
	// dimensionality. Used by the stub embedder; the gemini embedder
	// reads it as a request parameter. Zero leaves the provider's
	// default. Env override: SATELLITES_EMBEDDINGS_DIMENSION.
	EmbeddingsDimension int `toml:"embeddings_dimension"`

	// ManifestURL is the GitHub Release manifest URL the system_version
	// verb fetches to surface the latest published satellites-client
	// build stamp. Default points at the "latest" alias on the
	// satellites GitHub Releases surface; tests override this via the
	// SATELLITES_MANIFEST_URL env var (or TOML key) to point at a fake
	// httptest server. sty_64e69db8.
	ManifestURL string `toml:"manifest_url"`

	// loadedTOMLPath is set by Load() to the absolute path of the TOML
	// file that was actually read (empty when defaults+env supplied the
	// whole config). Read via the LoadedTOMLPath() accessor — the field
	// is unexported so it doesn't show up in Describe()/TOML serialisation.
	loadedTOMLPath string

	// jwtSecretGenerated is true when Load() filled JWTSecret with a
	// random per-boot value because env+TOML supplied none. collectWarnings
	// uses this to emit a prod-mode warning that restarts will invalidate
	// every minted MCP access token.
	jwtSecretGenerated bool
}

// LoadedTOMLPath returns the absolute path of the TOML file that the
// last Load() call actually read, or "" when the config came from
// defaults+env alone. Used at boot to log proof that the operator's
// TOML file was picked up; tests that mount a TOML at /app/satellites.toml
// assert on this in container.Logs to verify the loader path actually ran.
func (c *Config) LoadedTOMLPath() string {
	return c.loadedTOMLPath
}

// FieldDoc is one entry in the Describe() table — the single source of truth
// for the (config field, env var, default, prod-recommended) mapping.
type FieldDoc struct {
	// Field is the exported Go field name on Config.
	Field string
	// Env is the env var name read by Load() as an override on top of TOML
	// values and code defaults.
	Env string
	// Default is a human-readable rendering of the default.
	Default string
	// ProdRecommended is true when an empty value triggers a startup
	// warning under ENV=prod (e.g. DBDSN, APIKeys). The service still
	// boots; the warning surfaces the operator-fixable gap.
	ProdRecommended bool
	// Description is a one-line summary suitable for an --env-help dump.
	Description string
}

// describeTable is the canonical Config field documentation. Adding a new
// exported field to Config without adding an entry here trips the
// reflection-based doc-coverage test in config_test.go.
var describeTable = []FieldDoc{
	{Field: "Port", Env: "SATELLITES_PORT", Default: "8080", Description: "HTTP listen port (1..65535)."},
	{Field: "Env", Env: "SATELLITES_ENV", Default: "prod", Description: "Deployment environment: dev or prod."},
	{Field: "LogLevel", Env: "SATELLITES_LOG_LEVEL", Default: "info", Description: "Arbor log level: trace, debug, info, warn, error."},
	{Field: "DevMode", Env: "SATELLITES_DEV_MODE", Default: "false", Description: "Enables dev-mode quick-signin and DEV portal affordances."},
	{Field: "DBDSN", Env: "SATELLITES_DB_DSN", Default: "(empty — DB-backed verbs disabled)", ProdRecommended: true, Description: "SurrealDB connection string. Recommended in prod."},
	{Field: "DevUsername", Env: "SATELLITES_DEV_USERNAME", Default: "dev@satellites.local (when DevMode=true) / (empty)", Description: "Fixed credential username for DevMode signin."},
	{Field: "DevPassword", Env: "SATELLITES_DEV_PASSWORD", Default: "dev123 (when DevMode=true) / (empty)", Description: "Fixed credential password for DevMode signin. Never logged."},
	{Field: "GoogleClientID", Env: "SATELLITES_GOOGLE_CLIENT_ID", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client id for Google. Pair with SATELLITES_GOOGLE_CLIENT_SECRET."},
	{Field: "GoogleClientSecret", Env: "SATELLITES_GOOGLE_CLIENT_SECRET", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client secret for Google. Never logged."},
	{Field: "GithubClientID", Env: "SATELLITES_GITHUB_CLIENT_ID", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client id for GitHub. Pair with SATELLITES_GITHUB_CLIENT_SECRET."},
	{Field: "GithubClientSecret", Env: "SATELLITES_GITHUB_CLIENT_SECRET", Default: "(empty — provider disabled)", Description: "OAuth 2.0 client secret for GitHub. Never logged."},
	{Field: "OAuthRedirectBaseURL", Env: "SATELLITES_OAUTH_REDIRECT_BASE_URL", Default: "http://localhost:<Port> (DevMode=true) / (empty)", ProdRecommended: true, Description: "Base URL for OAuth callback redirects. Recommended in prod when any provider is configured."},
	{Field: "PublicURL", Env: "SATELLITES_PUBLIC_URL", Default: "(empty — derived mcp_url is empty; meta panel renders not-configured empty-state)", ProdRecommended: true, Description: "Externally-reachable base URL of the satellites server. Used to derive each project's MCP connection string."},
	{Field: "OAuthTokenCacheTTL", Env: "SATELLITES_OAUTH_TOKEN_CACHE_TTL", Default: "4h", Description: "How long the MCP-side OAuth token validator caches a successful provider lookup."},
	{Field: "JWTSecret", Env: "SATELLITES_JWT_SECRET", Default: "(empty — random per-boot, all MCP tokens invalidate on restart)", ProdRecommended: true, Description: "HMAC-SHA256 key signing satellites-issued OAuth access JWTs. Set a stable value in prod."},
	{Field: "OAuthIssuer", Env: "SATELLITES_OAUTH_ISSUER", Default: "(empty — derived from request host)", Description: "Absolute base URL announced in OAuth discovery metadata and JWT iss claim. Set when fronted by an opaque proxy."},
	{Field: "OAuthAccessTokenTTL", Env: "SATELLITES_OAUTH_ACCESS_TOKEN_TTL", Default: "1h", Description: "Lifetime of MCP access JWTs minted at /oauth/token. Range 1m..24h."},
	{Field: "OAuthRefreshTokenTTL", Env: "SATELLITES_OAUTH_REFRESH_TOKEN_TTL", Default: "168h (7d)", Description: "Lifetime of refresh tokens issued alongside access tokens. Range 1h..30d."},
	{Field: "OAuthCodeTTL", Env: "SATELLITES_OAUTH_CODE_TTL", Default: "10m", Description: "Lifetime of single-use authorization codes minted at /oauth/authorize."},
	{Field: "APIKeys", Env: "SATELLITES_API_KEYS", Default: "(empty — Bearer-API-key auth disabled)", ProdRecommended: true, Description: "Bearer tokens accepted on /mcp. TOML: native array. Env: comma-separated."},
	{Field: "DocsDir", Env: "SATELLITES_DOCS_DIR", Default: "/app/docs", Description: "Container-side docs volume path read by document_ingest_file."},
	{Field: "GrantsEnforced", Env: "SATELLITES_GRANTS_ENFORCED", Default: "false", Description: "When true, MCP verbs outside the bootstrap allowlist require a covering role-grant."},
	{Field: "GeminiAPIKey", Env: "SATELLITES_GEMINI_API_KEY", Default: "(empty — reviewer falls back to AcceptAll)", Description: "Google AI Studio key used by the close-time reviewer. Never logged."},
	{Field: "GeminiReviewModel", Env: "SATELLITES_GEMINI_REVIEW_MODEL", Default: "gemini-2.5-flash", Description: "Gemini model id for the close-time reviewer. Empty resolves to the package default."},
	{Field: "EmbeddingsProvider", Env: "SATELLITES_EMBEDDINGS_PROVIDER", Default: "(empty — none, semantic search disabled)", Description: "Embeddings backend selector: gemini, stub, none."},
	{Field: "EmbeddingsModel", Env: "SATELLITES_EMBEDDINGS_MODEL", Default: "(empty — provider default)", Description: "Provider-specific embeddings model id."},
	{Field: "EmbeddingsAPIKey", Env: "SATELLITES_EMBEDDINGS_API_KEY", Default: "(empty — required for provider=gemini)", Description: "Embeddings provider API key. Never logged."},
	{Field: "EmbeddingsBaseURL", Env: "SATELLITES_EMBEDDINGS_BASE_URL", Default: "(empty — provider canonical endpoint)", Description: "Embeddings provider HTTP base URL override."},
	{Field: "EmbeddingsDimension", Env: "SATELLITES_EMBEDDINGS_DIMENSION", Default: "0 (provider default)", Description: "Optional embeddings vector dimensionality override."},
	{Field: "ManifestURL", Env: "SATELLITES_MANIFEST_URL", Default: "https://github.com/bobmcallan/satellites/releases/latest/download/manifest.json", Description: "GitHub Release manifest URL fetched by the system_version verb."},
}

// Describe returns the canonical (Field, Env, Default, ProdRecommended,
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

// configPathEnv names the env var operators set to point at an explicit
// TOML file. When set and unreadable, Load() emits a warning and falls
// through to defaults+env (the service still boots).
const configPathEnv = "SATELLITES_CONFIG"

// defaultConfigFile is the file the loader checks when SATELLITES_CONFIG
// is unset. Absence is silent — operators can run on env+defaults alone.
const defaultConfigFile = "satellites.toml"

// Load resolves Config from defaults → TOML → env and returns the
// resulting Config alongside a slice of operator-facing warnings. The
// service ALWAYS boots: malformed values, parse errors, missing-but-
// recommended prod secrets — all degrade to a warning and the field
// falls back to its code default. Callers iterate the warnings and log
// them via arbor.
func Load() (*Config, []string) {
	cfg := defaults()
	var warnings []string

	overlay, tomlPath, tomlWarnings := readTOMLOverlay()
	warnings = append(warnings, tomlWarnings...)
	cfg.loadedTOMLPath = tomlPath
	envSetDevMode := overlay.applyTo(cfg, &warnings)

	// Env / DevMode resolution must settle before the dev-mode default
	// block so the right shape applies.
	if v := os.Getenv("SATELLITES_ENV"); v != "" {
		normalised := normaliseEnv(v)
		if normalised != "dev" && normalised != "prod" {
			warnings = append(warnings, fmt.Sprintf("SATELLITES_ENV=%q invalid (want dev|prod) — falling back to default %q", v, cfg.Env))
		} else {
			cfg.Env = normalised
		}
	}
	if v := os.Getenv("SATELLITES_DEV_MODE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("SATELLITES_DEV_MODE=%q unparseable as bool — falling back to default %t", v, cfg.DevMode))
		} else {
			cfg.DevMode = b
			envSetDevMode = true
		}
	}
	if !envSetDevMode {
		cfg.DevMode = cfg.Env == "dev"
	}

	// Dev-mode-only quick-signin defaults: only apply when DevMode is
	// active AND neither TOML nor env supplied a value. Production sets
	// neither default to keep secrets out of binaries.
	if cfg.DevMode {
		if cfg.DevUsername == "" {
			cfg.DevUsername = "dev@satellites.local"
		}
		if cfg.DevPassword == "" {
			cfg.DevPassword = "dev123"
		}
		if cfg.OAuthRedirectBaseURL == "" {
			cfg.OAuthRedirectBaseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
		}
	}

	applyEnvOverrides(cfg, &warnings)

	// JWT secret has a special degrade-to-default: empty after env+TOML
	// resolution → generate a random 32-byte hex key per boot. The
	// service stays up but every restart invalidates outstanding MCP
	// access tokens. collectWarnings emits a prod-mode warning below.
	if cfg.JWTSecret == "" {
		if random, err := generateRandomJWTSecret(); err == nil {
			cfg.JWTSecret = random
			cfg.jwtSecretGenerated = true
		}
	}

	warnings = append(warnings, cfg.collectWarnings()...)
	return cfg, warnings
}

// generateRandomJWTSecret returns a 32-byte random secret encoded as hex.
// Used as the volatile fallback when SATELLITES_JWT_SECRET is unset so
// the OAuth-AS can mint tokens out-of-the-box; collectWarnings flags
// this state in prod since restarts invalidate every minted token.
func generateRandomJWTSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// duration wraps time.Duration with a TextUnmarshaler so go-toml/v2 can
// decode Go-duration strings ("5m", "30s", "1h") into the OAuth TTL field.
type duration time.Duration

// UnmarshalText parses a Go duration string into the wrapper.
func (d *duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(parsed)
	return nil
}

// tomlOverlay is the TOML-decoding shadow of Config. Pointer fields let
// the loader distinguish "TOML set this" (non-nil) from "TOML didn't
// mention it" (nil), so the overlay can override defaults selectively
// without overwriting unrelated fields. Duration is wrapped to satisfy
// go-toml/v2, which lacks built-in time.Duration support.
type tomlOverlay struct {
	Port                 *int      `toml:"port"`
	Env                  *string   `toml:"env"`
	LogLevel             *string   `toml:"log_level"`
	DevMode              *bool     `toml:"dev_mode"`
	DBDSN                *string   `toml:"db_dsn"`
	DevUsername          *string   `toml:"dev_username"`
	DevPassword          *string   `toml:"dev_password"`
	GoogleClientID       *string   `toml:"google_client_id"`
	GoogleClientSecret   *string   `toml:"google_client_secret"`
	GithubClientID       *string   `toml:"github_client_id"`
	GithubClientSecret   *string   `toml:"github_client_secret"`
	OAuthRedirectBaseURL *string   `toml:"oauth_redirect_base_url"`
	PublicURL            *string   `toml:"public_url"`
	OAuthTokenCacheTTL   *duration `toml:"oauth_token_cache_ttl"`
	JWTSecret            *string   `toml:"jwt_secret"`
	OAuthIssuer          *string   `toml:"oauth_issuer"`
	OAuthAccessTokenTTL  *duration `toml:"oauth_access_token_ttl"`
	OAuthRefreshTokenTTL *duration `toml:"oauth_refresh_token_ttl"`
	OAuthCodeTTL         *duration `toml:"oauth_code_ttl"`
	APIKeys              []string  `toml:"api_keys"`
	DocsDir              *string   `toml:"docs_dir"`
	GrantsEnforced       *bool     `toml:"grants_enforced"`
	GeminiAPIKey         *string   `toml:"gemini_api_key"`
	GeminiReviewModel    *string   `toml:"gemini_review_model"`
	EmbeddingsProvider   *string   `toml:"embeddings_provider"`
	EmbeddingsModel      *string   `toml:"embeddings_model"`
	EmbeddingsAPIKey     *string   `toml:"embeddings_api_key"`
	EmbeddingsBaseURL    *string   `toml:"embeddings_base_url"`
	EmbeddingsDimension  *int      `toml:"embeddings_dimension"`
	ManifestURL          *string   `toml:"manifest_url"`
}

// applyTo copies overlay values into cfg for every non-nil field. Returns
// true when the overlay supplied a DevMode value so the caller can skip
// the env-derived default. Invalid TOML values (out-of-range port,
// unknown env, etc.) append to warnings and leave the field at its
// prior value.
func (o tomlOverlay) applyTo(cfg *Config, warnings *[]string) bool {
	if o.Port != nil {
		if *o.Port < 1 || *o.Port > 65535 {
			*warnings = append(*warnings, fmt.Sprintf("toml port=%d out of range — keeping %d", *o.Port, cfg.Port))
		} else {
			cfg.Port = *o.Port
		}
	}
	if o.Env != nil {
		normalised := normaliseEnv(*o.Env)
		if normalised != "dev" && normalised != "prod" {
			*warnings = append(*warnings, fmt.Sprintf("toml env=%q invalid (want dev|prod) — keeping %q", *o.Env, cfg.Env))
		} else {
			cfg.Env = normalised
		}
	}
	if o.LogLevel != nil {
		lvl := strings.ToLower(*o.LogLevel)
		if _, ok := validLogLevels[lvl]; !ok {
			*warnings = append(*warnings, fmt.Sprintf("toml log_level=%q invalid — keeping %q", *o.LogLevel, cfg.LogLevel))
		} else {
			cfg.LogLevel = lvl
		}
	}
	devModeSet := false
	if o.DevMode != nil {
		cfg.DevMode = *o.DevMode
		devModeSet = true
	}
	if o.DBDSN != nil {
		cfg.DBDSN = *o.DBDSN
	}
	if o.DevUsername != nil {
		cfg.DevUsername = *o.DevUsername
	}
	if o.DevPassword != nil {
		cfg.DevPassword = *o.DevPassword
	}
	if o.GoogleClientID != nil {
		cfg.GoogleClientID = *o.GoogleClientID
	}
	if o.GoogleClientSecret != nil {
		cfg.GoogleClientSecret = *o.GoogleClientSecret
	}
	if o.GithubClientID != nil {
		cfg.GithubClientID = *o.GithubClientID
	}
	if o.GithubClientSecret != nil {
		cfg.GithubClientSecret = *o.GithubClientSecret
	}
	if o.OAuthRedirectBaseURL != nil {
		cfg.OAuthRedirectBaseURL = *o.OAuthRedirectBaseURL
	}
	if o.PublicURL != nil {
		cfg.PublicURL = *o.PublicURL
	}
	if o.OAuthTokenCacheTTL != nil {
		d := time.Duration(*o.OAuthTokenCacheTTL)
		if d < time.Second || d > 24*time.Hour {
			*warnings = append(*warnings, fmt.Sprintf("toml oauth_token_cache_ttl=%s out of range (1s..24h) — keeping %s", d, cfg.OAuthTokenCacheTTL))
		} else {
			cfg.OAuthTokenCacheTTL = d
		}
	}
	if o.JWTSecret != nil {
		cfg.JWTSecret = *o.JWTSecret
	}
	if o.OAuthIssuer != nil {
		cfg.OAuthIssuer = *o.OAuthIssuer
	}
	if o.OAuthAccessTokenTTL != nil {
		d := time.Duration(*o.OAuthAccessTokenTTL)
		if d < time.Minute || d > 24*time.Hour {
			*warnings = append(*warnings, fmt.Sprintf("toml oauth_access_token_ttl=%s out of range (1m..24h) — keeping %s", d, cfg.OAuthAccessTokenTTL))
		} else {
			cfg.OAuthAccessTokenTTL = d
		}
	}
	if o.OAuthRefreshTokenTTL != nil {
		d := time.Duration(*o.OAuthRefreshTokenTTL)
		if d < time.Hour || d > 30*24*time.Hour {
			*warnings = append(*warnings, fmt.Sprintf("toml oauth_refresh_token_ttl=%s out of range (1h..30d) — keeping %s", d, cfg.OAuthRefreshTokenTTL))
		} else {
			cfg.OAuthRefreshTokenTTL = d
		}
	}
	if o.OAuthCodeTTL != nil {
		d := time.Duration(*o.OAuthCodeTTL)
		if d < 30*time.Second || d > 30*time.Minute {
			*warnings = append(*warnings, fmt.Sprintf("toml oauth_code_ttl=%s out of range (30s..30m) — keeping %s", d, cfg.OAuthCodeTTL))
		} else {
			cfg.OAuthCodeTTL = d
		}
	}
	if o.APIKeys != nil {
		cfg.APIKeys = append([]string(nil), o.APIKeys...)
	}
	if o.DocsDir != nil {
		cfg.DocsDir = *o.DocsDir
	}
	if o.GrantsEnforced != nil {
		cfg.GrantsEnforced = *o.GrantsEnforced
	}
	if o.GeminiAPIKey != nil {
		cfg.GeminiAPIKey = *o.GeminiAPIKey
	}
	if o.GeminiReviewModel != nil {
		cfg.GeminiReviewModel = *o.GeminiReviewModel
	}
	if o.EmbeddingsProvider != nil {
		cfg.EmbeddingsProvider = *o.EmbeddingsProvider
	}
	if o.EmbeddingsModel != nil {
		cfg.EmbeddingsModel = *o.EmbeddingsModel
	}
	if o.EmbeddingsAPIKey != nil {
		cfg.EmbeddingsAPIKey = *o.EmbeddingsAPIKey
	}
	if o.EmbeddingsBaseURL != nil {
		cfg.EmbeddingsBaseURL = *o.EmbeddingsBaseURL
	}
	if o.EmbeddingsDimension != nil {
		cfg.EmbeddingsDimension = *o.EmbeddingsDimension
	}
	if o.ManifestURL != nil {
		cfg.ManifestURL = *o.ManifestURL
	}
	return devModeSet
}

// readTOMLOverlay resolves the TOML path, parses the file if present, and
// returns the overlay alongside the path that was actually read and any
// warnings generated. An empty path means no TOML was loaded (the
// silent-when-absent default-config case); callers stamp this onto
// Config.loadedTOMLPath so the boot log can prove the loader ran.
//
// Failure modes (missing explicit file, parse error) emit a warning and
// return an empty overlay — the service still boots on defaults+env.
func readTOMLOverlay() (tomlOverlay, string, []string) {
	var overlay tomlOverlay
	var warnings []string
	path, explicit := resolveConfigPath()
	if path == "" {
		return overlay, "", warnings
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return overlay, "", warnings
		}
		warnings = append(warnings, fmt.Sprintf("config file %s unreadable: %v — falling back to defaults+env", path, err))
		return overlay, "", warnings
	}
	if err := toml.Unmarshal(raw, &overlay); err != nil {
		warnings = append(warnings, fmt.Sprintf("config file %s parse failed: %v — falling back to defaults+env", path, err))
		return tomlOverlay{}, "", warnings
	}
	return overlay, path, warnings
}

// defaults builds the in-code default Config. This is the lowest-precedence
// layer; TOML and env vars override it. Defaults are tuned for prod so a
// zero-config boot is safe in production; dev runs supply ENV=dev (or use
// the bundled dev TOML) to flip on quick-signin and the dev affordances.
func defaults() *Config {
	return &Config{
		Port:                 8080,
		Env:                  "prod",
		LogLevel:             "info",
		DevMode:              false,
		DBDSN:                "",
		DocsDir:              "/app/docs",
		OAuthTokenCacheTTL:   4 * time.Hour,
		OAuthAccessTokenTTL:  1 * time.Hour,
		OAuthRefreshTokenTTL: 7 * 24 * time.Hour,
		OAuthCodeTTL:         10 * time.Minute,
		GeminiReviewModel:    "gemini-2.5-flash",
		EmbeddingsProvider:   "none",
		ManifestURL:          "https://github.com/bobmcallan/satellites/releases/latest/download/manifest.json",
	}
}

// resolveConfigPath returns (path, explicit). When SATELLITES_CONFIG is
// set, the path is explicit. Otherwise the loader looks for
// ./satellites.toml; an absent default file returns path="".
func resolveConfigPath() (string, bool) {
	if p := strings.TrimSpace(os.Getenv(configPathEnv)); p != "" {
		return p, true
	}
	if _, err := os.Stat(defaultConfigFile); err == nil {
		return defaultConfigFile, false
	}
	return "", false
}

// applyEnvOverrides mutates cfg in place with values from the process env.
// Each override is gated on a non-empty env var; empty (or unset) leaves
// the prior value intact. Parse errors and out-of-range values append to
// warnings and leave the field at its prior value — Load() never returns
// a fatal error.
func applyEnvOverrides(cfg *Config, warnings *[]string) {
	if v := os.Getenv("SATELLITES_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		switch {
		case err != nil:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_PORT=%q unparseable as int — keeping %d", v, cfg.Port))
		case p < 1 || p > 65535:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_PORT=%d out of range (1..65535) — keeping %d", p, cfg.Port))
		default:
			cfg.Port = p
		}
	}
	if v := os.Getenv("SATELLITES_LOG_LEVEL"); v != "" {
		lvl := strings.ToLower(v)
		if _, ok := validLogLevels[lvl]; !ok {
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_LOG_LEVEL=%q invalid — keeping %q", v, cfg.LogLevel))
		} else {
			cfg.LogLevel = lvl
		}
	}
	if v := os.Getenv("SATELLITES_DB_DSN"); v != "" {
		cfg.DBDSN = v
	}
	if v := os.Getenv("SATELLITES_DEV_USERNAME"); v != "" {
		cfg.DevUsername = v
	}
	if v := os.Getenv("SATELLITES_DEV_PASSWORD"); v != "" {
		cfg.DevPassword = v
	}
	if v := os.Getenv("SATELLITES_GOOGLE_CLIENT_ID"); v != "" {
		cfg.GoogleClientID = v
	}
	if v := os.Getenv("SATELLITES_GOOGLE_CLIENT_SECRET"); v != "" {
		cfg.GoogleClientSecret = v
	}
	if v := os.Getenv("SATELLITES_GITHUB_CLIENT_ID"); v != "" {
		cfg.GithubClientID = v
	}
	if v := os.Getenv("SATELLITES_GITHUB_CLIENT_SECRET"); v != "" {
		cfg.GithubClientSecret = v
	}
	if v := os.Getenv("SATELLITES_OAUTH_REDIRECT_BASE_URL"); v != "" {
		cfg.OAuthRedirectBaseURL = v
	}
	if v := os.Getenv("SATELLITES_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("SATELLITES_OAUTH_TOKEN_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_TOKEN_CACHE_TTL=%q unparseable as duration — keeping %s", v, cfg.OAuthTokenCacheTTL))
		case d < time.Second || d > 24*time.Hour:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_TOKEN_CACHE_TTL=%s out of range (1s..24h) — keeping %s", d, cfg.OAuthTokenCacheTTL))
		default:
			cfg.OAuthTokenCacheTTL = d
		}
	}
	if v := os.Getenv("SATELLITES_JWT_SECRET"); v != "" {
		cfg.JWTSecret = v
	}
	if v := os.Getenv("SATELLITES_OAUTH_ISSUER"); v != "" {
		cfg.OAuthIssuer = v
	}
	if v := os.Getenv("SATELLITES_OAUTH_ACCESS_TOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_ACCESS_TOKEN_TTL=%q unparseable as duration — keeping %s", v, cfg.OAuthAccessTokenTTL))
		case d < time.Minute || d > 24*time.Hour:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_ACCESS_TOKEN_TTL=%s out of range (1m..24h) — keeping %s", d, cfg.OAuthAccessTokenTTL))
		default:
			cfg.OAuthAccessTokenTTL = d
		}
	}
	if v := os.Getenv("SATELLITES_OAUTH_REFRESH_TOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_REFRESH_TOKEN_TTL=%q unparseable as duration — keeping %s", v, cfg.OAuthRefreshTokenTTL))
		case d < time.Hour || d > 30*24*time.Hour:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_REFRESH_TOKEN_TTL=%s out of range (1h..30d) — keeping %s", d, cfg.OAuthRefreshTokenTTL))
		default:
			cfg.OAuthRefreshTokenTTL = d
		}
	}
	if v := os.Getenv("SATELLITES_OAUTH_CODE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_CODE_TTL=%q unparseable as duration — keeping %s", v, cfg.OAuthCodeTTL))
		case d < 30*time.Second || d > 30*time.Minute:
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_OAUTH_CODE_TTL=%s out of range (30s..30m) — keeping %s", d, cfg.OAuthCodeTTL))
		default:
			cfg.OAuthCodeTTL = d
		}
	}
	if v := os.Getenv("SATELLITES_API_KEYS"); v != "" {
		var keys []string
		for _, part := range strings.Split(v, ",") {
			if k := strings.TrimSpace(part); k != "" {
				keys = append(keys, k)
			}
		}
		cfg.APIKeys = keys
	}
	if v := os.Getenv("SATELLITES_DOCS_DIR"); v != "" {
		cfg.DocsDir = v
	}
	if v := os.Getenv("SATELLITES_GRANTS_ENFORCED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_GRANTS_ENFORCED=%q unparseable as bool — keeping %t", v, cfg.GrantsEnforced))
		} else {
			cfg.GrantsEnforced = b
		}
	}
	if v := os.Getenv("SATELLITES_GEMINI_API_KEY"); v != "" {
		cfg.GeminiAPIKey = v
	}
	if v := os.Getenv("SATELLITES_GEMINI_REVIEW_MODEL"); v != "" {
		cfg.GeminiReviewModel = v
	}
	if v := os.Getenv("SATELLITES_EMBEDDINGS_PROVIDER"); v != "" {
		cfg.EmbeddingsProvider = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("SATELLITES_EMBEDDINGS_MODEL"); v != "" {
		cfg.EmbeddingsModel = v
	}
	if v := os.Getenv("SATELLITES_EMBEDDINGS_API_KEY"); v != "" {
		cfg.EmbeddingsAPIKey = v
	}
	if v := os.Getenv("SATELLITES_EMBEDDINGS_BASE_URL"); v != "" {
		cfg.EmbeddingsBaseURL = v
	}
	if v := os.Getenv("SATELLITES_EMBEDDINGS_DIMENSION"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("SATELLITES_EMBEDDINGS_DIMENSION=%q unparseable as int — keeping %d", v, cfg.EmbeddingsDimension))
		} else {
			cfg.EmbeddingsDimension = d
		}
	}
	if v := os.Getenv("SATELLITES_MANIFEST_URL"); v != "" {
		cfg.ManifestURL = v
	}
}

// collectWarnings inspects the resolved Config and appends one warning
// per missing-but-recommended prod knob. OAuth pair mismatches disable
// the affected provider and warn rather than failing boot.
func (c *Config) collectWarnings() []string {
	var out []string
	if c.Env == "prod" && strings.TrimSpace(c.DBDSN) == "" {
		out = append(out, "SATELLITES_DB_DSN empty under ENV=prod — DB-backed verbs disabled (no story persistence)")
	}
	if c.Env == "prod" && len(c.APIKeys) == 0 {
		out = append(out, "SATELLITES_API_KEYS empty under ENV=prod — Bearer-API-key auth disabled on /mcp")
	}
	if c.Env == "prod" && c.jwtSecretGenerated {
		out = append(out, "SATELLITES_JWT_SECRET unset under ENV=prod — generated random per-boot key; MCP access tokens will be invalidated on every restart")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		out = append(out, fmt.Sprintf("Google OAuth half-set (id=%t, secret=%t) — provider disabled", c.GoogleClientID != "", c.GoogleClientSecret != ""))
		c.GoogleClientID = ""
		c.GoogleClientSecret = ""
	}
	if (c.GithubClientID == "") != (c.GithubClientSecret == "") {
		out = append(out, fmt.Sprintf("GitHub OAuth half-set (id=%t, secret=%t) — provider disabled", c.GithubClientID != "", c.GithubClientSecret != ""))
		c.GithubClientID = ""
		c.GithubClientSecret = ""
	}
	hasOAuth := c.GoogleClientID != "" || c.GithubClientID != ""
	if c.Env == "prod" && hasOAuth && strings.TrimSpace(c.OAuthRedirectBaseURL) == "" {
		out = append(out, "SATELLITES_OAUTH_REDIRECT_BASE_URL empty under ENV=prod with OAuth configured — callbacks will fail until set")
	}
	return out
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
