package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ternarybob/arbor"
	"golang.org/x/oauth2"

	"github.com/bobmcallan/satellites/internal/config"
)

// ProviderUserInfo is the minimal identity the auth layer needs from an OAuth
// provider — just enough to upsert a user. Providers map their own payloads
// into this shape via FetchUserInfo.
type ProviderUserInfo struct {
	Email       string
	DisplayName string
}

// Provider ties an oauth2.Config + a provider-specific userinfo fetcher to a
// human-readable name used in routes + user.Provider.
type Provider struct {
	Name      string // "google" or "github"
	OAuth2    *oauth2.Config
	FetchInfo func(ctx context.Context, token *oauth2.Token) (ProviderUserInfo, error)
}

// ProviderSet holds the enabled providers. Missing provider configs (e.g.
// GOOGLE_CLIENT_ID=="") leave the provider nil, which disables its routes.
type ProviderSet struct {
	Google *Provider
	GitHub *Provider
}

// Enabled returns the list of active providers in name order.
func (p *ProviderSet) Enabled() []*Provider {
	out := make([]*Provider, 0, 2)
	if p.Google != nil {
		out = append(out, p.Google)
	}
	if p.GitHub != nil {
		out = append(out, p.GitHub)
	}
	return out
}

// StateStore is the server-side CSRF state-token registry. States are minted
// on /start and consumed on /callback; expired entries are pruned lazily.
type StateStore struct {
	mu   sync.Mutex
	byID map[string]time.Time
	ttl  time.Duration
	now  func() time.Time
}

// NewStateStore returns an empty store with ttl (10 minutes is the common
// default).
func NewStateStore(ttl time.Duration) *StateStore {
	return &StateStore{
		byID: make(map[string]time.Time),
		ttl:  ttl,
		now:  time.Now,
	}
}

// Mint generates a new random state token and records its expiry.
func (s *StateStore) Mint() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: read random: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	s.byID[id] = s.now().Add(s.ttl)
	s.mu.Unlock()
	return id, nil
}

// Consume returns nil if id is present and not expired; the row is deleted so
// a replay fails. Returns an error otherwise.
func (s *StateStore) Consume(id string) error {
	if id == "" {
		return errors.New("oauth: empty state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.byID[id]
	if !ok {
		return errors.New("oauth: unknown state")
	}
	delete(s.byID, id)
	if s.now().After(exp) {
		return errors.New("oauth: expired state")
	}
	return nil
}

// BuildProviderSet constructs ProviderSet from the validated Config. Providers
// with any missing credential are left nil (disabled).
func BuildProviderSet(cfg *config.Config) *ProviderSet {
	base := cfg.OAuthRedirectBaseURL
	set := &ProviderSet{}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" && base != "" {
		set.Google = &Provider{
			Name: "google",
			OAuth2: &oauth2.Config{
				ClientID:     cfg.GoogleClientID,
				ClientSecret: cfg.GoogleClientSecret,
				RedirectURL:  base + "/auth/google/callback",
				Scopes:       []string{"openid", "email", "profile"},
				Endpoint: oauth2.Endpoint{
					AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
					TokenURL: "https://oauth2.googleapis.com/token",
				},
			},
			FetchInfo: fetchGoogleUserInfo,
		}
	}
	if cfg.GithubClientID != "" && cfg.GithubClientSecret != "" && base != "" {
		set.GitHub = &Provider{
			Name: "github",
			OAuth2: &oauth2.Config{
				ClientID:     cfg.GithubClientID,
				ClientSecret: cfg.GithubClientSecret,
				RedirectURL:  base + "/auth/github/callback",
				Scopes:       []string{"user:email"},
				Endpoint: oauth2.Endpoint{
					AuthURL:  "https://github.com/login/oauth/authorize",
					TokenURL: "https://github.com/login/oauth/access_token",
				},
			},
			FetchInfo: fetchGitHubUserInfo,
		}
	}
	return set
}

// RegisterOAuth attaches per-provider /start + /callback routes on mux. Only
// enabled providers register; callers of disabled providers get 404 from the
// mux itself.
func (h *Handlers) RegisterOAuth(mux *http.ServeMux) {
	if h.Providers == nil {
		return
	}
	for _, p := range h.Providers.Enabled() {
		provider := p // capture
		mux.HandleFunc("GET /auth/"+provider.Name+"/start", func(w http.ResponseWriter, r *http.Request) {
			h.oauthStart(w, r, provider)
		})
		mux.HandleFunc("GET /auth/"+provider.Name+"/callback", func(w http.ResponseWriter, r *http.Request) {
			h.oauthCallback(w, r, provider)
		})
	}
}

func (h *Handlers) oauthStart(w http.ResponseWriter, r *http.Request, p *Provider) {
	state, err := h.States.Mint()
	if err != nil {
		h.Logger.Error().Str("provider", p.Name).Str("error", err.Error()).Msg("oauth state mint failed")
		http.Error(w, "state mint failed", http.StatusInternalServerError)
		return
	}
	url := p.OAuth2.AuthCodeURL(state, oauth2.AccessTypeOnline)
	h.Logger.Info().Str("event", "oauth-start").Str("provider", p.Name).Msg("oauth start")
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (h *Handlers) oauthCallback(w http.ResponseWriter, r *http.Request, p *Provider) {
	state := r.URL.Query().Get("state")
	if err := h.States.Consume(state); err != nil {
		h.Logger.Warn().
			Str("event", "oauth-callback-fail").
			Str("provider", p.Name).
			Str("reason", "state_invalid").
			Msg("oauth callback rejected")
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		h.Logger.Warn().
			Str("event", "oauth-callback-fail").
			Str("provider", p.Name).
			Str("reason", "missing_code").
			Msg("oauth callback rejected")
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	token, err := p.OAuth2.Exchange(r.Context(), code)
	if err != nil {
		h.Logger.Warn().
			Str("event", "oauth-callback-fail").
			Str("provider", p.Name).
			Str("reason", "exchange_failed").
			Msg("oauth callback rejected")
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	info, err := p.FetchInfo(r.Context(), token)
	if err != nil {
		h.Logger.Warn().
			Str("event", "oauth-callback-fail").
			Str("provider", p.Name).
			Str("reason", "userinfo_failed").
			Msg("oauth callback rejected")
		http.Error(w, "userinfo fetch failed", http.StatusBadGateway)
		return
	}
	if info.Email == "" {
		h.Logger.Warn().
			Str("event", "oauth-callback-fail").
			Str("provider", p.Name).
			Str("reason", "no_email").
			Msg("oauth callback rejected")
		http.Error(w, "no verified email", http.StatusBadRequest)
		return
	}

	user := h.upsertOAuthUser(p.Name, info)
	sess, err := h.Sessions.Create(user.ID, DefaultSessionTTL)
	if err != nil {
		h.Logger.Error().Str("error", err.Error()).Msg("oauth session create failed")
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	WriteCookie(w, sess, cookieOpts(h.Cfg))
	h.Logger.Info().
		Str("event", "oauth-callback-success").
		Str("provider", p.Name).
		Str("user_id", user.ID).
		Msg("oauth callback success")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// upsertOAuthUser is idempotent: a second call with the same provider+email
// returns the existing user row rather than minting a new ID. Runs against
// any UserStore implementation (Memory + Surreal) — story_7512783a widened
// the surface so OAuth login persists across Fly restarts.
func (h *Handlers) upsertOAuthUser(provider string, info ProviderUserInfo) User {
	key := provider + ":" + normaliseEmail(info.Email)
	if h.Users == nil {
		return User{ID: "u_" + key, Email: key, DisplayName: info.DisplayName, Provider: provider}
	}
	if existing, err := h.Users.GetByEmail(key); err == nil {
		return existing
	}
	u := User{
		ID:          "u_" + key,
		Email:       key,
		DisplayName: info.DisplayName,
		Provider:    provider,
	}
	h.Users.Add(u)
	if h.OnUserCreated != nil {
		h.OnUserCreated(context.Background(), u.ID)
	}
	return u
}

// ensure we use arbor for the type assertion in signature
var _ arbor.ILogger = (arbor.ILogger)(nil)
