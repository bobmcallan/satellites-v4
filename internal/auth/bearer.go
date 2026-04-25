// BearerValidator validates OAuth provider access tokens against Google's
// userinfo endpoint and GitHub's /user endpoint. It's the consumer-side
// validator for /mcp's OAuth Bearer auth path (story_512cc5cd) — it does
// NOT mint tokens and does NOT act as an OAuth provider.
//
// In addition to provider tokens, the validator accepts satellites-signed
// bearer tokens (prefix "sat_") issued by the portal's
// /auth/token/exchange endpoint. These are looked up in an in-memory
// registry rather than the network — they're how harness/CLI clients
// driven by a signed-in human get programmatic access to /mcp.

package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// satelliteBearerPrefix marks an opaque token issued by /auth/token/exchange
// rather than fetched from a third-party OAuth provider. The validator
// short-circuits to its in-memory registry when it sees this prefix.
const satelliteBearerPrefix = "sat_"

// BearerInfo is the minimal identity returned by a successful Validate
// call. UserID is the local satellites user id (provider:email shape for
// OAuth-validated tokens, the underlying session's UserID for satellite-
// signed bearers).
type BearerInfo struct {
	UserID   string
	Email    string
	Provider string // "google" | "github" | "satellites"
}

// BearerValidatorConfig captures the knobs the validator reads from cfg.
type BearerValidatorConfig struct {
	// CacheTTL is how long a successful provider lookup stays in the
	// cache before re-validating.
	CacheTTL time.Duration
	// CacheMax bounds the cache size; 0 disables the cap (not
	// recommended). Default 1024.
	CacheMax int
	// HTTPClient is the client used for provider calls. nil → DefaultClient.
	HTTPClient *http.Client
	// GoogleUserinfoURL / GithubUserURL override the default endpoints
	// for tests. Empty → production defaults.
	GoogleUserinfoURL string
	GithubUserURL     string
}

// BearerValidator caches successful token validations and routes lookups
// to the correct provider. Safe for concurrent use.
type BearerValidator struct {
	cfg BearerValidatorConfig

	mu       sync.Mutex
	cache    map[string]bearerCacheEntry
	registry map[string]bearerCacheEntry // satellites-signed bearers
	now      func() time.Time
}

type bearerCacheEntry struct {
	info      BearerInfo
	expiresAt time.Time
}

// NewBearerValidator constructs a validator with the provided config.
// CacheTTL <= 0 falls back to a 5-minute default; CacheMax <= 0 falls
// back to 1024.
func NewBearerValidator(cfg BearerValidatorConfig) *BearerValidator {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.CacheMax <= 0 {
		cfg.CacheMax = 1024
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.GoogleUserinfoURL == "" {
		cfg.GoogleUserinfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"
	}
	if cfg.GithubUserURL == "" {
		cfg.GithubUserURL = "https://api.github.com/user"
	}
	return &BearerValidator{
		cfg:      cfg,
		cache:    make(map[string]bearerCacheEntry),
		registry: make(map[string]bearerCacheEntry),
		now:      time.Now,
	}
}

// Validate takes an opaque bearer token, returns the resolved identity on
// success, or an error if no provider accepts it.
func (b *BearerValidator) Validate(ctx context.Context, token string) (BearerInfo, error) {
	if token == "" {
		return BearerInfo{}, errors.New("bearer: empty token")
	}

	// Satellites-signed bearer: registry lookup.
	if strings.HasPrefix(token, satelliteBearerPrefix) {
		b.mu.Lock()
		entry, ok := b.registry[token]
		if ok && b.now().Before(entry.expiresAt) {
			b.mu.Unlock()
			return entry.info, nil
		}
		if ok {
			delete(b.registry, token)
		}
		b.mu.Unlock()
		return BearerInfo{}, errors.New("bearer: unknown or expired sat token")
	}

	// Cache hit?
	b.mu.Lock()
	entry, ok := b.cache[token]
	if ok && b.now().Before(entry.expiresAt) {
		b.mu.Unlock()
		return entry.info, nil
	}
	if ok {
		// Expired — purge.
		delete(b.cache, token)
	}
	b.mu.Unlock()

	// Try Google then GitHub. First success wins.
	if info, err := b.validateGoogle(ctx, token); err == nil {
		b.cachePut(token, info)
		return info, nil
	}
	if info, err := b.validateGithub(ctx, token); err == nil {
		b.cachePut(token, info)
		return info, nil
	}
	return BearerInfo{}, errors.New("bearer: no provider accepted the token")
}

// IssueSatelliteBearer mints a sat_-prefixed opaque token for the given
// user identity, valid for ttl. Used by /auth/token/exchange.
func (b *BearerValidator) IssueSatelliteBearer(info BearerInfo, ttl time.Duration) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("bearer: read random: %w", err)
	}
	token := satelliteBearerPrefix + base64.RawURLEncoding.EncodeToString(buf)
	b.mu.Lock()
	b.registry[token] = bearerCacheEntry{
		info:      info,
		expiresAt: b.now().Add(ttl),
	}
	b.mu.Unlock()
	return token, nil
}

func (b *BearerValidator) cachePut(token string, info BearerInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cache) >= b.cfg.CacheMax {
		// Evict the closest-to-expiring entry.
		var oldest string
		var oldestAt time.Time
		first := true
		for k, e := range b.cache {
			if first || e.expiresAt.Before(oldestAt) {
				oldest = k
				oldestAt = e.expiresAt
				first = false
			}
		}
		if oldest != "" {
			delete(b.cache, oldest)
		}
	}
	b.cache[token] = bearerCacheEntry{
		info:      info,
		expiresAt: b.now().Add(b.cfg.CacheTTL),
	}
}

func (b *BearerValidator) validateGoogle(ctx context.Context, token string) (BearerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.GoogleUserinfoURL, nil)
	if err != nil {
		return BearerInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := b.cfg.HTTPClient.Do(req)
	if err != nil {
		return BearerInfo{}, fmt.Errorf("google userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return BearerInfo{}, fmt.Errorf("google userinfo: status %d", resp.StatusCode)
	}
	var body struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return BearerInfo{}, fmt.Errorf("google userinfo decode: %w", err)
	}
	if body.Email == "" {
		return BearerInfo{}, errors.New("google userinfo: no email")
	}
	return BearerInfo{
		UserID:   "u_google:" + normaliseEmail(body.Email),
		Email:    "google:" + normaliseEmail(body.Email),
		Provider: "google",
	}, nil
}

func (b *BearerValidator) validateGithub(ctx context.Context, token string) (BearerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.GithubUserURL, nil)
	if err != nil {
		return BearerInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := b.cfg.HTTPClient.Do(req)
	if err != nil {
		return BearerInfo{}, fmt.Errorf("github user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return BearerInfo{}, fmt.Errorf("github user: status %d", resp.StatusCode)
	}
	var body struct {
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return BearerInfo{}, fmt.Errorf("github user decode: %w", err)
	}
	id := body.Email
	if id == "" {
		id = body.Login
	}
	if id == "" {
		return BearerInfo{}, errors.New("github user: no identifier")
	}
	return BearerInfo{
		UserID:   "u_github:" + id,
		Email:    "github:" + id,
		Provider: "github",
	}, nil
}
