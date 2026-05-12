package clicred_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/clicred"
)

func TestResolve_FlagWins(t *testing.T) {
	t.Setenv(clicred.EnvToken, "env-token")
	tok, err := clicred.Resolve("flag-token", "https://example")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "flag-token" {
		t.Fatalf("flag did not win: %q", tok)
	}
}

func TestResolve_EnvWhenNoFlag(t *testing.T) {
	t.Setenv(clicred.EnvToken, "env-token")
	tok, err := clicred.Resolve("", "https://example")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "env-token" {
		t.Fatalf("env did not win: %q", tok)
	}
}

func TestResolve_FileWhenNoFlagOrEnv(t *testing.T) {
	t.Setenv(clicred.EnvToken, "")
	dir := t.TempDir()
	credPath := filepath.Join(dir, ".satellites", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(credPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(credPath, []byte(`{"https://example": "file-token", "default": "default-token"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", dir)
	tok, err := clicred.Resolve("", "https://example")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "file-token" {
		t.Fatalf("file did not win: %q", tok)
	}
}

func TestResolve_FileFallbackToDefault(t *testing.T) {
	t.Setenv(clicred.EnvToken, "")
	dir := t.TempDir()
	credPath := filepath.Join(dir, ".satellites", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(credPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(credPath, []byte(`{"default": "default-token"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", dir)
	tok, err := clicred.Resolve("", "https://unmatched")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "default-token" {
		t.Fatalf("default did not win: %q", tok)
	}
}

func TestResolve_MissingEverything(t *testing.T) {
	t.Setenv(clicred.EnvToken, "")
	dir := t.TempDir() // no creds file inside
	t.Setenv("HOME", dir)
	if _, err := clicred.Resolve("", "https://example"); !errors.Is(err, clicred.ErrNoToken) {
		t.Fatalf("Resolve missing chain returned: %v", err)
	}
}

func TestResolveWithTOML_TOMLBetweenEnvAndFile(t *testing.T) {
	t.Setenv(clicred.EnvToken, "")
	dir := t.TempDir()
	credPath := filepath.Join(dir, ".satellites", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(credPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(credPath, []byte(`{"https://example": "json-token"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", dir)

	// TOML beats credentials.json when set.
	tok, err := clicred.ResolveWithTOML("", "toml-token", "https://example")
	if err != nil {
		t.Fatalf("ResolveWithTOML: %v", err)
	}
	if tok != "toml-token" {
		t.Fatalf("TOML did not beat JSON: %q", tok)
	}

	// JSON wins when TOML is empty.
	tok, err = clicred.ResolveWithTOML("", "", "https://example")
	if err != nil {
		t.Fatalf("ResolveWithTOML: %v", err)
	}
	if tok != "json-token" {
		t.Fatalf("JSON should win when TOML empty: %q", tok)
	}
}

func TestWriteToken_FreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".satellites", "credentials.json")
	if err := clicred.WriteToken(path, "https://example", "tok-1"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), `"https://example"`) || !strings.Contains(string(raw), `"tok-1"`) {
		t.Fatalf("written file missing expected entries: %s", string(raw))
	}
}

func TestWriteToken_UpsertPreservesSiblings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".satellites", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"https://alpha":"alpha-tok","https://beta":"beta-tok"}`), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := clicred.WriteToken(path, "https://alpha", "alpha-2"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"alpha-2"`) {
		t.Fatalf("alpha entry not upserted: %s", body)
	}
	if !strings.Contains(body, `"beta-tok"`) {
		t.Fatalf("beta sibling clobbered: %s", body)
	}
}

func TestWriteToken_Perms0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".satellites", "credentials.json")
	if err := clicred.WriteToken(path, "https://example", "tok-1"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perms = %o, want 0600", perm)
	}
}

func TestWriteToken_RoundTripWithResolve(t *testing.T) {
	t.Setenv(clicred.EnvToken, "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".satellites", "credentials.json")
	if err := clicred.WriteToken(path, "https://example", "tok-rt"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	tok, err := clicred.Resolve("", "https://example")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "tok-rt" {
		t.Fatalf("round-trip token = %q, want tok-rt", tok)
	}
}

func TestResolveWithTOML_FlagAndEnvStillBeatTOML(t *testing.T) {
	t.Setenv(clicred.EnvToken, "env-token")
	tok, err := clicred.ResolveWithTOML("flag-token", "toml-token", "https://example")
	if err != nil {
		t.Fatalf("ResolveWithTOML: %v", err)
	}
	if tok != "flag-token" {
		t.Fatalf("flag should win: %q", tok)
	}

	tok, err = clicred.ResolveWithTOML("", "toml-token", "https://example")
	if err != nil {
		t.Fatalf("ResolveWithTOML: %v", err)
	}
	if tok != "env-token" {
		t.Fatalf("env should beat TOML: %q", tok)
	}
}
