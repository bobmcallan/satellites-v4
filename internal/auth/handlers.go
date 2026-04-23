package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/config"
)

// Handlers holds the dependencies shared by Login and Logout.
type Handlers struct {
	Users     UserStoreByEmail
	Sessions  SessionStore
	Logger    arbor.ILogger
	Cfg       *config.Config
	Providers *ProviderSet // optional; set to wire /auth/<provider>/start + /callback
	States    *StateStore  // required if Providers is non-nil
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
			// Ensure downstream session → user lookups resolve. The
			// MemoryUserStore composes by id + email; register once on
			// first DevMode login.
			if ms, ok := h.Users.(*MemoryUserStore); ok {
				if _, err := ms.GetByID(u.ID); err != nil {
					ms.Add(u)
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
