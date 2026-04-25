package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubProvider returns a httptest.Server that emulates Google's userinfo
// AND GitHub's /user endpoints. Routes by path. Tracks call counts per
// route via the returned counters.
func stubProvider(t *testing.T) (server *httptest.Server, googleCalls, githubCalls *atomic.Int64) {
	t.Helper()
	googleCalls = &atomic.Int64{}
	githubCalls = &atomic.Int64{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		switch r.URL.Path {
		case "/google":
			googleCalls.Add(1)
			if token != "good-google-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": "1234", "email": "alice@example.com"})
		case "/github":
			githubCalls.Add(1)
			if token != "good-github-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "bob", "email": "bob@example.com"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, googleCalls, githubCalls
}

func newTestValidator(t *testing.T) (*BearerValidator, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	stub, gCalls, hCalls := stubProvider(t)
	v := NewBearerValidator(BearerValidatorConfig{
		CacheTTL:          time.Minute,
		CacheMax:          8,
		GoogleUserinfoURL: stub.URL + "/google",
		GithubUserURL:     stub.URL + "/github",
	})
	return v, gCalls, hCalls
}

func TestBearerValidator_Google(t *testing.T) {
	v, _, _ := newTestValidator(t)
	info, err := v.Validate(context.Background(), "good-google-tok")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if info.Provider != "google" {
		t.Errorf("Provider = %q, want google", info.Provider)
	}
	if info.Email != "google:alice@example.com" {
		t.Errorf("Email = %q", info.Email)
	}
}

func TestBearerValidator_Github(t *testing.T) {
	v, _, _ := newTestValidator(t)
	info, err := v.Validate(context.Background(), "good-github-tok")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if info.Provider != "github" {
		t.Errorf("Provider = %q, want github", info.Provider)
	}
	if info.Email != "github:bob@example.com" {
		t.Errorf("Email = %q", info.Email)
	}
}

func TestBearerValidator_InvalidToken(t *testing.T) {
	v, _, _ := newTestValidator(t)
	if _, err := v.Validate(context.Background(), "garbage"); err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestBearerValidator_EmptyToken(t *testing.T) {
	v, _, _ := newTestValidator(t)
	if _, err := v.Validate(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestBearerValidator_CacheHit(t *testing.T) {
	v, gCalls, _ := newTestValidator(t)
	for i := 0; i < 3; i++ {
		if _, err := v.Validate(context.Background(), "good-google-tok"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := gCalls.Load(); got != 1 {
		t.Errorf("google call count = %d, want 1 (cache should serve calls 2 and 3)", got)
	}
}

func TestBearerValidator_CacheExpiryReValidates(t *testing.T) {
	v, gCalls, _ := newTestValidator(t)
	v.cfg.CacheTTL = 10 * time.Millisecond
	if _, err := v.Validate(context.Background(), "good-google-tok"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := v.Validate(context.Background(), "good-google-tok"); err != nil {
		t.Fatal(err)
	}
	if got := gCalls.Load(); got != 2 {
		t.Errorf("google call count = %d, want 2 (cache must re-validate after TTL)", got)
	}
}

func TestBearerValidator_LRUEvict(t *testing.T) {
	v, _, _ := newTestValidator(t)
	v.cfg.CacheMax = 2

	// Two distinct successful tokens — should fit.
	for i := 0; i < 5; i++ {
		token := "good-google-tok"
		// Mix in an invalid token to vary cache keys without changing
		// stub behaviour (only good tokens enter the cache).
		_ = token
	}
	// Drive two valid tokens to fill the cache, then a third should
	// evict the oldest entry.
	v.cache["a"] = bearerCacheEntry{info: BearerInfo{Email: "a"}, expiresAt: v.now().Add(time.Minute)}
	v.cache["b"] = bearerCacheEntry{info: BearerInfo{Email: "b"}, expiresAt: v.now().Add(2 * time.Minute)}
	v.cachePut("c", BearerInfo{Email: "c"})
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) != 2 {
		t.Errorf("cache size = %d, want 2 (evict cap)", len(v.cache))
	}
	if _, exists := v.cache["a"]; exists {
		t.Errorf("oldest entry 'a' should have been evicted")
	}
}

func TestBearerValidator_SatelliteBearer(t *testing.T) {
	v, _, _ := newTestValidator(t)
	tok, err := v.IssueSatelliteBearer(BearerInfo{
		UserID: "u_alice", Email: "alice@local", Provider: "satellites",
	}, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.HasPrefix(tok, satelliteBearerPrefix) {
		t.Errorf("token = %q, want sat_ prefix", tok)
	}
	info, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if info.UserID != "u_alice" {
		t.Errorf("UserID = %q, want u_alice", info.UserID)
	}
}

func TestBearerValidator_SatelliteBearer_Expired(t *testing.T) {
	v, _, _ := newTestValidator(t)
	tok, err := v.IssueSatelliteBearer(BearerInfo{UserID: "u_alice"}, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("expected error on expired sat token")
	}
}
