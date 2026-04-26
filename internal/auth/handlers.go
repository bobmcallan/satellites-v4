package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/config"
)

// Handlers holds the dependencies shared by Login and Logout.
type Handlers struct {
	Users     UserStore
	Sessions  SessionStore
	Logger    arbor.ILogger
	Cfg       *config.Config
	Providers *ProviderSet // optional; set to wire /auth/<provider>/start + /callback
	States    *StateStore  // required if Providers is non-nil

	// OnUserCreated fires once per first-sight of a user id, from either
	// DevMode or an OAuth callback. Wired by main() to seed the user's
	// default workspace per docs/architecture.md §8 (Workspace is the
	// multi-tenant primitive). Optional; nil = no-op.
	OnUserCreated func(ctx context.Context, userID string)

	// LoginLimiter throttles per-IP credential submissions on POST
	// /auth/login. Optional — nil disables the gate (test default).
	// story_d5652302 wires the live limiter from main().
	LoginLimiter LoginLimiter
}

// LoginLimiter is the narrow surface auth.Handlers consumes from
// internal/ratelimit. Kept as an interface so tests can inject a
// deterministic stub without depending on the package.
type LoginLimiter interface {
	Allow(key string) bool
}

// UserStoreByEmail is the lookup surface the login handler needs. Kept
// separate from UserStoreByID so mocks can satisfy just one.
type UserStoreByEmail interface {
	GetByEmail(email string) (User, error)
}

// Register attaches login + logout + enabled OAuth provider routes to mux.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/login", h.Login)
	mux.HandleFunc("POST /auth/logout", h.Logout)
	h.RegisterOAuth(mux)
}

// Login authenticates the supplied username + password, creates a session,
// sets the cookie, and redirects 303 to `/` (or to `?next=` when present).
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	if h.LoginLimiter != nil {
		key := loginRateKey(r)
		if !h.LoginLimiter.Allow(key) {
			h.Logger.Warn().Str("event", "login-throttled").Str("source_ip", key).Msg("auth login rate-limited")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")
	if next == "" {
		next = r.URL.Query().Get("next")
	}
	if next == "" {
		next = "/"
	}

	h.Logger.Info().Str("event", "login-attempt").Str("username", username).Msg("auth login attempt")

	user, ok := h.authenticate(username, password)
	if !ok {
		h.Logger.Info().Str("event", "login-fail").Str("username", username).Msg("auth login failed")
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	sess, err := h.Sessions.Create(user.ID, DefaultSessionTTL)
	if err != nil {
		h.Logger.Error().Str("error", err.Error()).Msg("session create failed")
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	WriteCookie(w, sess, cookieOpts(h.Cfg))
	h.Logger.Info().
		Str("event", "login-success").
		Str("user_id", user.ID).
		Str("session_id", sess.ID).
		Msg("auth login success")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// Logout clears the session cookie and the backing store row, then redirects
// 303 to /login.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	id := ReadCookie(r)
	if id != "" {
		_ = h.Sessions.Delete(id)
	}
	ClearCookie(w, cookieOpts(h.Cfg))
	h.Logger.Info().Str("event", "logout").Str("session_id", id).Msg("auth logout")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// authenticate tries DevMode first (when allowed), then bcrypt. Never logs
// the password.
func (h *Handlers) authenticate(username, password string) (User, bool) {
	// DevMode is gated on cfg.DevMode AND cfg.Env != "prod" — prod is a
	// hard deny regardless of DEV_MODE env.
	if h.Cfg.Env != "prod" && h.Cfg.DevMode && h.Cfg.DevUsername != "" && username == h.Cfg.DevUsername {
		if password != "" && password == h.Cfg.DevPassword {
			u := User{
				ID:          "dev-user",
				Email:       h.Cfg.DevUsername,
				DisplayName: "Dev User",
				Provider:    "devmode",
			}
			// Ensure downstream session → user lookups resolve. Add on
			// first DevMode login; the underlying UserStore handles its
			// own dedupe via GetByID. Story_7512783a widened the field
			// type so this path runs against Memory + Surreal stores.
			if h.Users != nil {
				if _, err := h.Users.GetByID(u.ID); err != nil {
					h.Users.Add(u)
					if h.OnUserCreated != nil {
						h.OnUserCreated(context.Background(), u.ID)
					}
				}
			}
			return u, true
		}
		return User{}, false
	}
	if h.Cfg.Env == "prod" && h.Cfg.DevMode {
		// In prod, DevMode is disabled even when DEV_MODE=true was mis-set.
		// Fall through to bcrypt only.
	}
	user, err := h.Users.GetByEmail(username)
	if err != nil {
		// Intentionally do nothing — indistinguishable from password mismatch.
		return User{}, false
	}
	if err := VerifyPassword(user.HashedPassword, password); err != nil {
		if errors.Is(err, ErrPasswordMismatch) {
			return User{}, false
		}
		h.Logger.Error().Str("error", err.Error()).Msg("auth bcrypt verify error")
		return User{}, false
	}
	return user, true
}

func cookieOpts(cfg *config.Config) CookieOptions {
	return CookieOptions{Secure: cfg.Env == "prod"}
}

// loginRateKey resolves the per-request rate-limit key. Behind Fly's
// proxy the original client IP is delivered in X-Forwarded-For; fall
// back to RemoteAddr (host portion) when absent. Empty string is a
// stable bucket — denying-all-when-unparseable is preferable to
// silently letting an unidentified caller bypass the limit.
func loginRateKey(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Format: "client, proxy1, proxy2"; take the first hop.
		if idx := strings.IndexByte(xff, ','); idx > 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if r.RemoteAddr == "" {
		return ""
	}
	if idx := strings.LastIndexByte(r.RemoteAddr, ':'); idx > 0 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}
