package config

import (
	"reflect"
	"testing"
	"time"
)

func TestLoad_Happy_DevDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.Env != "dev" {
		t.Errorf("Env = %q, want \"dev\"", cfg.Env)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want \"info\"", cfg.LogLevel)
	}
	if !cfg.DevMode {
		t.Errorf("DevMode = false, want true (default dev env)")
	}
	if cfg.DevUsername != "dev@satellites.local" {
		t.Errorf("DevUsername = %q, want \"dev@satellites.local\"", cfg.DevUsername)
	}
	if cfg.DevPassword != "dev123" {
		t.Errorf("DevPassword = %q, want \"dev123\"", cfg.DevPassword)
	}
	if cfg.OAuthRedirectBaseURL != "http://localhost:8080" {
		t.Errorf("OAuthRedirectBaseURL = %q, want \"http://localhost:8080\"", cfg.OAuthRedirectBaseURL)
	}
	if cfg.OAuthTokenCacheTTL != 5*time.Minute {
		t.Errorf("OAuthTokenCacheTTL = %s, want 5m", cfg.OAuthTokenCacheTTL)
	}
	if cfg.DocsDir != "/app/docs" {
		t.Errorf("DocsDir = %q, want \"/app/docs\"", cfg.DocsDir)
	}
	if cfg.GrantsEnforced {
		t.Errorf("GrantsEnforced = true, want false")
	}
}

// TestDescribe_CoversAllFields walks the Config struct via reflection and
// asserts each exported field has a matching entry in Describe(). Trips when
// a new field is added to Config without a describeTable entry.
func TestDescribe_CoversAllFields(t *testing.T) {
	docs := Describe()
	docByField := make(map[string]FieldDoc, len(docs))
	for _, d := range docs {
		docByField[d.Field] = d
	}

	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, ok := docByField[f.Name]; !ok {
			t.Errorf("Config.%s has no Describe() entry — add one to describeTable", f.Name)
		}
	}

	// Reverse direction: every Describe() entry must name a real field.
	for _, d := range docs {
		if _, ok := rt.FieldByName(d.Field); !ok {
			t.Errorf("describeTable references missing field %q", d.Field)
		}
	}
}

// TestDescribe_AllFieldsCovered asserts each Describe() entry has either a
// non-empty Default OR ProdRequired=true. AC2's "default OR prod-required"
// rule is the substantive form of "every field is configured for both
// environments".
func TestDescribe_AllFieldsCovered(t *testing.T) {
	for _, d := range Describe() {
		if d.Default == "" && !d.ProdRequired {
			t.Errorf("Field %q has empty Default and ProdRequired=false — pick one", d.Field)
		}
		if d.Env == "" {
			t.Errorf("Field %q has empty Env", d.Field)
		}
		if d.Description == "" {
			t.Errorf("Field %q has empty Description", d.Field)
		}
	}
}

func TestLoad_OAuthPartialCreds(t *testing.T) {
	cases := []struct {
		name string
		envs map[string]string
	}{
		{"google id only", map[string]string{"GOOGLE_CLIENT_ID": "x"}},
		{"google secret only", map[string]string{"GOOGLE_CLIENT_SECRET": "y"}},
		{"github id only", map[string]string{"GITHUB_CLIENT_ID": "x"}},
		{"github secret only", map[string]string{"GITHUB_CLIENT_SECRET": "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.envs {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("Load() = nil, want error on partial OAuth creds")
			}
		})
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("LOG_LEVEL", "bogus")
	if _, err := Load(); err == nil {
		t.Fatalf("Load() = nil, want error on invalid LOG_LEVEL")
	}
}

func TestLoad_OAuthCacheTTLOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("OAUTH_TOKEN_CACHE_TTL", "30s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.OAuthTokenCacheTTL != 30*time.Second {
		t.Errorf("OAuthTokenCacheTTL = %s, want 30s", cfg.OAuthTokenCacheTTL)
	}

	clearEnv(t)
	t.Setenv("OAUTH_TOKEN_CACHE_TTL", "garbage")
	if _, err := Load(); err == nil {
		t.Fatalf("Load() = nil, want error on garbage TTL")
	}

	clearEnv(t)
	t.Setenv("OAUTH_TOKEN_CACHE_TTL", "0s")
	if _, err := Load(); err == nil {
		t.Fatalf("Load() = nil, want error on zero TTL (out of range)")
	}
}

func TestLoad_ProdSecretsAreEmpty(t *testing.T) {
	clearEnv(t)
	t.Setenv("ENV", "prod")
	t.Setenv("DB_DSN", "ws://db.internal:8000/rpc")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.DevUsername != "" {
		t.Errorf("DevUsername = %q in prod, want empty", cfg.DevUsername)
	}
	if cfg.DevPassword != "" {
		t.Errorf("DevPassword = %q in prod, want empty", cfg.DevPassword)
	}
	if cfg.OAuthRedirectBaseURL != "" {
		t.Errorf("OAuthRedirectBaseURL = %q in prod (no override), want empty", cfg.OAuthRedirectBaseURL)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("ENV", "prod")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("DB_DSN", "ws://db.internal:8000/rpc")
	t.Setenv("FLY_MACHINE_ID", "1234abcd")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.Env != "prod" {
		t.Errorf("Env = %q, want \"prod\"", cfg.Env)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want \"debug\"", cfg.LogLevel)
	}
	if cfg.DBDSN != "ws://db.internal:8000/rpc" {
		t.Errorf("DBDSN = %q", cfg.DBDSN)
	}
	if cfg.FlyMachineID != "1234abcd" {
		t.Errorf("FlyMachineID = %q", cfg.FlyMachineID)
	}
	if cfg.DevMode {
		t.Errorf("DevMode = true, want false in prod with default DEV_MODE")
	}
}

func TestLoad_MissingDBDSN_InProd(t *testing.T) {
	clearEnv(t)
	t.Setenv("ENV", "prod")
	_, err := Load()
	if err == nil {
		t.Fatalf("Load() = nil, want error about DB_DSN")
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "abc")
	if _, err := Load(); err == nil {
		t.Fatalf("Load() = nil, want error on non-numeric PORT")
	}

	t.Setenv("PORT", "99999")
	if _, err := Load(); err == nil {
		t.Fatalf("Load() = nil, want error on out-of-range PORT")
	}
}

func TestLoad_InvalidEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("ENV", "staging")
	if _, err := Load(); err == nil {
		t.Fatalf("Load() = nil, want error on unknown ENV")
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORT", "SATELLITES_PORT", "ENV", "LOG_LEVEL", "DEV_MODE", "DB_DSN",
		"FLY_MACHINE_ID", "DEV_USERNAME", "DEV_PASSWORD",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET",
		"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET",
		"OAUTH_REDIRECT_BASE_URL", "OAUTH_TOKEN_CACHE_TTL",
		"SATELLITES_API_KEYS", "DOCS_DIR", "SATELLITES_GRANTS_ENFORCED",
	} {
		t.Setenv(k, "")
	}
}
