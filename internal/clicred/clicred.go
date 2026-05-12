// Package clicred resolves the bearer token for `satellites-client`
// invocations. Resolution chain (highest priority first):
//
//  1. --token <t> flag
//  2. SATELLITES_TOKEN env var
//  3. bin/satellites-client.toml (loaded by internal/config.LoadClient;
//     passed in via ResolveWithTOML)
//  4. ~/.satellites/credentials.json (per-server keyed)
//
// Per docs/cli-primary-design.md §3 ("Credential resolution").
//
// The credentials file shape:
//
//	{
//	  "https://satellites-pprod.fly.dev": "sat_xxx",
//	  "https://localhost:8080": "sat_yyy"
//	}
//
// One key per server URL; values are bearers. Falls back to the
// "default" key when the supplied server URL has no entry.
package clicred

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnvToken is the env var read in step 2.
const EnvToken = "SATELLITES_TOKEN"

// DefaultCredentialsPath returns ~/.satellites/credentials.json,
// resolved relative to the caller's home directory. Returns an
// error if the home dir is not resolvable (rare; the OS reports
// it via os.UserHomeDir).
func DefaultCredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".satellites", "credentials.json"), nil
}

// ErrNoToken is returned by Resolve when no token is found in any
// of the three sources. The caller (Cobra root PreRun) maps it to
// cliexit.Auth.
var ErrNoToken = errors.New("no bearer token found (set --token, SATELLITES_TOKEN, populate bin/satellites-client.toml, or populate ~/.satellites/credentials.json)")

// Resolve returns the bearer for the given server URL. flagToken
// wins; then env; then the credentials file. Returns ErrNoToken
// when none match. Equivalent to ResolveWithTOML(flagToken, "", server).
func Resolve(flagToken, server string) (string, error) {
	return ResolveWithTOML(flagToken, "", server)
}

// ResolveWithTOML extends Resolve with a TOML-supplied token that
// slots between env and credentials.json. The caller (typically
// cmd/satellites-client/main.go's PreRun) loads bin/satellites-client.toml
// via internal/config.LoadClient and threads the token in here so the
// precedence chain stays linear.
func ResolveWithTOML(flagToken, tomlToken, server string) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if env := os.Getenv(EnvToken); env != "" {
		return env, nil
	}
	if tomlToken != "" {
		return tomlToken, nil
	}
	path, err := DefaultCredentialsPath()
	if err != nil {
		return "", err
	}
	tok, err := readCredentialsFile(path, server)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoToken
		}
		return "", fmt.Errorf("read credentials: %w", err)
	}
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

// WriteToken upserts the bearer for the given server into the
// credentials file at path. Reads the existing map (preserving other
// servers' entries) if present, then writes the merged map back with
// 0o600 perms. Creates the parent directory at 0o700 if missing.
// Idempotent — writing the same bearer twice is observable as the
// same file contents.
func WriteToken(path, server, bearer string) error {
	if server == "" {
		return errors.New("WriteToken: server must not be empty")
	}
	if bearer == "" {
		return errors.New("WriteToken: bearer must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	creds := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		if jerr := json.Unmarshal(raw, &creds); jerr != nil {
			return fmt.Errorf("parse %s: %w", path, jerr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	creds[server] = bearer
	encoded, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readCredentialsFile reads the json file at path and returns the
// bearer for the given server. Falls back to the "default" key
// when the server URL has no matching entry.
func readCredentialsFile(path, server string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var creds map[string]string
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if tok, ok := creds[server]; ok {
		return tok, nil
	}
	if tok, ok := creds["default"]; ok {
		return tok, nil
	}
	return "", nil
}
