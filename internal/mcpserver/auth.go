package mcpserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/auth"
)

type ctxKey int

const userKey ctxKey = iota

// CallerIdentity is what handlers read from ctx: the resolved user's email
// (for sessions) or a synthetic api-key id.
type CallerIdentity struct {
	Email  string
	Source string // "session" | "apikey"
}

// UserFrom returns the caller identity attached by AuthMiddleware.
func UserFrom(ctx context.Context) (CallerIdentity, bool) {
	v, ok := ctx.Value(userKey).(CallerIdentity)
	return v, ok
}

// AuthDeps are the satellites dependencies the middleware needs to resolve
// a caller.
type AuthDeps struct {
	Sessions auth.SessionStore
	Users    auth.UserStoreByID
	APIKeys  []string
	Logger   arbor.ILogger
}

// AuthMiddleware wraps next with /mcp authentication: either a valid
// satellites_session cookie OR an Authorization: Bearer <api-key> matching
// one of the configured APIKeys. Unauth requests get 401 + WWW-Authenticate.
func AuthMiddleware(deps AuthDeps) func(http.Handler) http.Handler {
	keyset := make(map[string]struct{}, len(deps.APIKeys))
	for _, k := range deps.APIKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keyset[k] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Bearer API key.
			if token := bearerToken(r); token != "" {
				if _, ok := keyset[token]; ok {
					ctx := context.WithValue(r.Context(), userKey, CallerIdentity{
						Email:  "apikey",
						Source: "apikey",
					})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			// Session cookie.
			if id := auth.ReadCookie(r); id != "" {
				sess, err := deps.Sessions.Get(id)
				if err == nil {
					user, err := deps.Users.GetByID(sess.UserID)
					if err == nil {
						ctx := context.WithValue(r.Context(), userKey, CallerIdentity{
							Email:  user.Email,
							Source: "session",
						})
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="satellites"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
		})
	}
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
