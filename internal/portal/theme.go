// Theme picker (story_5dd7167a). The portal stores the user's theme
// preference as a server-readable cookie so the SSR template can render
// the correct `data-theme` on first paint. Modes: "dark" | "light" |
// "system". Absence defaults to "dark".

package portal

import (
	"net/http"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
)

const (
	themeCookieName = "satellites_theme"
	themeCookieTTL  = 365 * 24 * time.Hour
	themeDefault    = "dark"
)

// validThemeModes lists the three modes the picker writes. Any other cookie
// value is treated as the default (dark).
var validThemeModes = map[string]struct{}{
	"dark":   {},
	"light":  {},
	"system": {},
}

// themeFromRequest reads the satellites_theme cookie and returns the stored
// mode. Returns the default (`dark`) when the cookie is missing or
// unreadable.
func themeFromRequest(r *http.Request) string {
	c, err := r.Cookie(themeCookieName)
	if err != nil || c == nil {
		return themeDefault
	}
	v := strings.TrimSpace(c.Value)
	if _, ok := validThemeModes[v]; !ok {
		return themeDefault
	}
	return v
}

// resolveTheme converts a stored mode into the literal `data-theme` value
// that the template renders on `<html>`. The "system" mode falls back to
// dark on SSR (no Accept-CH for prefers-color-scheme yet); the inline
// first-paint script in head.html re-resolves on the client when the mode
// is "system" via matchMedia.
func resolveTheme(mode string) string {
	switch mode {
	case "light":
		return "light"
	case "dark":
		return "dark"
	default:
		// "system" or unknown → dark on the server; the client script
		// flips to light when prefers-color-scheme matches.
		return "dark"
	}
}

// handleThemeSet accepts POST /theme with `mode` and `next` form fields.
// Validates the mode, writes the cookie, and 302s to `next` (defaulting to
// `/`). Mode validation rejects anything outside the validThemeModes set
// with a 400.
func (p *Portal) handleThemeSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	if _, ok := validThemeModes[mode]; !ok {
		http.Error(w, "invalid mode", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookieName,
		Value:    mode,
		Path:     "/",
		MaxAge:   int(themeCookieTTL.Seconds()),
		SameSite: http.SameSiteLaxMode,
		HttpOnly: false, // readable by the inline first-paint script
	})
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// settingsData feeds settings.html. Mirrors the other portal-page view
// models so the shared head/nav templates render with the same fields.
type settingsData struct {
	Title           string
	Version         string
	Commit          string
	User            auth.User
	Workspaces      []wsChip
	ActiveWorkspace wsChip
	DevMode         bool
	ThemeMode       string
	ThemePickerNext string
	WSConfig        WSConfig
}

// handleSettings renders the user-facing settings page (story_ccee859d).
// The theme picker now lives here instead of inside the hamburger
// dropdown; the dropdown links to /settings.
func (p *Portal) handleSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := p.resolveUser(r)
	if !ok {
		p.redirectToLogin(w, r)
		return
	}
	active, chips, _ := p.activeWorkspace(r, user)
	data := settingsData{
		Title:           "settings",
		Version:         config.Version,
		Commit:          config.GitCommit,
		User:            user,
		Workspaces:      chips,
		ActiveWorkspace: active,
		DevMode:         p.cfg.Env != "prod" && p.cfg.DevMode,
		ThemeMode:       themeFromRequest(r),
		ThemePickerNext: "/settings",
		WSConfig:        buildWSConfig(active, r),
	}
	if err := p.tmpl.ExecuteTemplate(w, "settings.html", data); err != nil {
		p.logger.Error().Str("template", "settings.html").Str("error", err.Error()).Msg("template render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}
