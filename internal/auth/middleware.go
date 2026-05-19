// Package auth — bearer + session authentication for satellites-server.
//
// CallerIdentity is the resolved caller bound to ctx by either the
// AuthMiddleware bearer pipeline (env keyset → agent_apikey → OAuth →
// cookie) or RequireSession's cookie-only path. Handlers read it via
// UserFrom regardless of which path placed it. Source carries the path
// that resolved.
//
// Relocated from internal/mcpserver per sty_f2d342e2 (cli-primary
// order:07a) so the new /api/v1/* HTTP surface and the existing /mcp
// surface share one bearer pipeline. pr_no_unrequested_compat: the
// previous mcpserver definitions were deleted in the same change, no
// alias layer.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ternarybob/arbor"
)

type ctxKey int

const userKey ctxKey = iota

// CallerIdentity is what handlers read from ctx: the resolved user's email
// (for sessions) or a synthetic api-key id. UserID is the stable opaque
// identifier used by project/document ownership — sess.UserID for session
// callers, the literal "apikey" for env-keyset api-key callers, the
// substrate row's owner UserID for agent_apikey callers.
//
// sty_056b68f6: TaskID + AllowedVerbs are populated when the caller's
// token is a task-scoped api-key row. AssertVerbAllowed(ctx, verb)
// reads AllowedVerbs to gate verb invocations; project-scoped keys /
// OAuth / session callers leave both fields zero and bypass the gate.
type CallerIdentity struct {
	Email  string
	UserID string
	Source string // "session" | "apikey" | "apikey:<id>" | "oauth:<provider>"
	// GlobalAdmin is set when the resolved user has the persisted flag
	// or matches SATELLITES_GLOBAL_ADMIN_EMAILS. story_3548cde2.
	GlobalAdmin bool
	// TaskID is the task this caller's api-key is scoped to (empty for
	// project-scoped keys / OAuth / session). sty_056b68f6.
	TaskID string
	// AllowedVerbs is the per-task verb allowlist (nil for un-narrowed
	// callers — they bypass the verb gate). sty_056b68f6.
	AllowedVerbs []string
}

// UserFrom returns the caller identity attached by AuthMiddleware /
// RequireSession.
func UserFrom(ctx context.Context) (CallerIdentity, bool) {
	v, ok := ctx.Value(userKey).(CallerIdentity)
	return v, ok
}

// WithCaller returns a copy of ctx carrying the supplied identity. Used
// by tests + by alternate transports (e.g. internal callers that have
// already resolved a caller without running AuthMiddleware).
func WithCaller(ctx context.Context, id CallerIdentity) context.Context {
	return context.WithValue(ctx, userKey, id)
}

// AuthDeps are the satellites dependencies the bearer middleware needs.
type AuthDeps struct {
	Sessions       SessionStore
	Users          UserStoreByID
	APIKeys        []string
	APIKeyStore    APIKeyStore
	Logger         arbor.ILogger
	OAuthValidator *BearerValidator // optional; nil skips OAuth-Bearer path
}

// AuthMiddleware wraps next with /mcp + /api/v1 authentication. Four paths
// in order (story_512cc5cd; sty_3191fbfc; sty_f2d342e2):
//
//  1. Authorization: Bearer <api-key> matching env-var APIKeys.
//  2. Authorization: Bearer <token> matching an active agent_apikey row.
//  3. Authorization: Bearer <token> validated by OAuthValidator.
//  4. satellites_session cookie.
//
// Unauthenticated requests get 401 + WWW-Authenticate.
func AuthMiddleware(deps AuthDeps) func(http.Handler) http.Handler {
	keyset := make(map[string]struct{}, len(deps.APIKeys))
	for _, k := range deps.APIKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keyset[k] = struct{}{}
		}
	}
	adminEmails := LoadGlobalAdminEmails()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token != "" {
				// 1. Bearer API key (env-var keyset).
				if _, ok := keyset[token]; ok {
					ctx := context.WithValue(r.Context(), userKey, CallerIdentity{
						Email:  "apikey",
						UserID: "apikey",
						Source: "apikey",
					})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				// 2. Substrate agent_apikey store (sty_3191fbfc).
				if deps.APIKeyStore != nil {
					if key, err := deps.APIKeyStore.LookupByToken(r.Context(), token); err == nil {
						owner, _ := deps.Users.GetByID(key.OwnerUserID)
						caller := CallerIdentity{
							Email:  owner.Email,
							UserID: key.OwnerUserID,
							Source: "apikey:" + key.ID,
							// sty_056b68f6: stamp task-scoped fields so the
							// AssertVerbAllowed gate can read them on this
							// request's ctx. Project-scoped keys leave both
							// zero (TaskID == "" → gate bypass).
							TaskID:       key.TaskID,
							AllowedVerbs: key.AllowedVerbs,
						}
						caller.GlobalAdmin = IsGlobalAdmin(owner, adminEmails)
						ctx := context.WithValue(r.Context(), userKey, caller)
						go func(id string) {
							_ = deps.APIKeyStore.Touch(context.Background(), id, time.Now().UTC())
						}(key.ID)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
				// 3. OAuth bearer (Google / GitHub / satellites-signed).
				if deps.OAuthValidator != nil {
					if info, err := deps.OAuthValidator.Validate(r.Context(), token); err == nil {
						caller := CallerIdentity{
							Email:  info.Email,
							UserID: info.UserID,
							Source: "oauth:" + info.Provider,
						}
						caller.GlobalAdmin = IsGlobalAdmin(User{Email: info.Email}, adminEmails)
						ctx := context.WithValue(r.Context(), userKey, caller)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}
			// 4. Session cookie.
			if id := ReadCookie(r); id != "" {
				sess, err := deps.Sessions.Get(id)
				if err == nil {
					user, err := deps.Users.GetByID(sess.UserID)
					if err == nil {
						caller := CallerIdentity{
							Email:  user.Email,
							UserID: user.ID,
							Source: "session",
						}
						caller.GlobalAdmin = IsGlobalAdmin(user, adminEmails)
						ctx := context.WithValue(r.Context(), userKey, caller)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}
			// RFC 9728: point clients at the protected-resource metadata
			// endpoint so the MCP SDK can discover the authorization server.
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`,
				SchemeAndHost(r),
			))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
		})
	}
}

// RequireSession is middleware that resolves the session cookie to a user
// and attaches CallerIdentity{Source:"session"} to the request context.
// Unauthenticated requests are redirected to /login with the original URL
// preserved in `?next=`.
func RequireSession(sessions SessionStore, users UserStoreByID, opts CookieOptions) func(http.Handler) http.Handler {
	adminEmails := LoadGlobalAdminEmails()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ReadCookie(r)
			if id == "" {
				redirectToLogin(w, r)
				return
			}
			sess, err := sessions.Get(id)
			if err != nil {
				ClearCookie(w, opts)
				redirectToLogin(w, r)
				return
			}
			user, err := users.GetByID(sess.UserID)
			if err != nil {
				_ = sessions.Delete(sess.ID)
				ClearCookie(w, opts)
				redirectToLogin(w, r)
				return
			}
			caller := CallerIdentity{
				Email:       user.Email,
				UserID:      user.ID,
				Source:      "session",
				GlobalAdmin: IsGlobalAdmin(user, adminEmails),
			}
			ctx := context.WithValue(r.Context(), userKey, caller)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserStoreByID is the lookup surface RequireSession + AuthMiddleware need.
type UserStoreByID interface {
	GetByID(id string) (User, error)
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	loc := "/login?next=" + url.QueryEscape(next)
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// SchemeAndHost reconstructs the absolute base URL from the request,
// honouring X-Forwarded-Proto so requests reaching the binary over Fly's
// edge TLS resolve to https rather than the plaintext scheme the binary
// itself terminates.
func SchemeAndHost(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
