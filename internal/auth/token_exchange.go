package auth

import (
	"encoding/json"
	"net/http"
	"time"
)

// SatelliteBearerTTL is how long a token minted via /auth/token/exchange
// remains valid. Short by design — clients should re-exchange when the
// token expires.
const SatelliteBearerTTL = 30 * time.Minute

// TokenExchange wires a /auth/token/exchange POST handler that mints a
// satellites-signed bearer token (sat_-prefixed) for the calling
// session. story_512cc5cd: gives portal-signed-in clients a programmatic
// token they can present on /mcp.
type TokenExchange struct {
	Sessions  SessionStore
	Users     UserStoreByID
	Validator *BearerValidator
}

// Register attaches the POST /auth/token/exchange handler to mux.
func (t *TokenExchange) Register(mux *http.ServeMux) {
	if t == nil || t.Validator == nil {
		return
	}
	mux.HandleFunc("POST /auth/token/exchange", t.handle)
}

func (t *TokenExchange) handle(w http.ResponseWriter, r *http.Request) {
	id := ReadCookie(r)
	if id == "" {
		http.Error(w, "session required", http.StatusUnauthorized)
		return
	}
	sess, err := t.Sessions.Get(id)
	if err != nil {
		http.Error(w, "session invalid", http.StatusUnauthorized)
		return
	}
	user, err := t.Users.GetByID(sess.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusUnauthorized)
		return
	}
	tok, err := t.Validator.IssueSatelliteBearer(BearerInfo{
		UserID:   user.ID,
		Email:    user.Email,
		Provider: "satellites",
	}, SatelliteBearerTTL)
	if err != nil {
		http.Error(w, "issue failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      tok,
		"expires_in": int(SatelliteBearerTTL.Seconds()),
		"token_type": "Bearer",
	})
}
